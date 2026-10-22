//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// TestModelCopyProgressSurvivesServerRestart crosses real process, WebSocket,
// storage and API boundaries. Local directories exercise copy wiring; explicit
// L4 verifies NFS reads without subjecting a production export to faults.
func TestModelCopyProgressSurvivesServerRestart(t *testing.T) {
	sourceRoot, targetRoot := t.TempDir(), t.TempDir()
	config := serverFixtureConfig(t, serverconfig.File{StorageProbeRoots: []string{sourceRoot, targetRoot}})
	h := newStreamHarnessWithConfig(t, config)
	t.Cleanup(func() {
		if t.Failed() {
			for _, path := range []string{h.serverLog, h.agentLog} {
				data, _ := os.ReadFile(path)
				t.Logf("%s:\n%s", path, data)
			}
		}
	})
	post := func(path string, value any) []byte {
		response, body := authedDo(t, http.MethodPost, h.base, h.adminToken, path, mustJSON(t, value))
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			t.Fatalf("POST %s: %d %s", path, response.StatusCode, body)
		}
		return body
	}
	createStorage := func(name, path string) uint {
		body := post("/api/v1/storages", map[string]any{"name": name, "type": "local", "path": path, "enabled": true, "all_nodes": true})
		var result struct {
			ID uint `json:"id"`
		}
		if err := json.Unmarshal(body, &result); err != nil || result.ID == 0 {
			t.Fatalf("storage: %s %v", body, err)
		}
		return result.ID
	}
	sourceStorage, targetStorage := createStorage("copy-source", sourceRoot), createStorage("copy-target", targetRoot)
	source := filepath.Join(sourceRoot, "tiny")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.json"), []byte(`{"model_type":"qwen3","architectures":["Qwen3ForCausalLM"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	weights := bytes.Repeat([]byte("copy-content"), 1<<20)
	if err := os.WriteFile(filepath.Join(source, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	body := post("/api/v1/models/local", map[string]any{"node_uuid": h.node.UUID, "storage_id": sourceStorage, "path": source, "name": "copy-progress"})
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(body, &model); err != nil || model.ID == 0 {
		t.Fatalf("model: %s %v", body, err)
	}
	post(fmt.Sprintf("/api/v1/models/%d/copy", model.ID), map[string]any{
		"source": map[string]any{"storage_id": sourceStorage, "node_uuid": h.node.UUID},
		"target": map[string]any{"storage_id": targetStorage, "node_uuid": h.node.UUID}, "rate_limit_kib_s": 1024,
	})
	type presence struct {
		StorageID  uint   `json:"storage_id"`
		State      string `json:"state"`
		Downloaded int64  `json:"downloaded"`
		Size       int64  `json:"size_bytes"`
		Path       string `json:"path"`
	}
	read := func() presence {
		response, body := authedDo(t, http.MethodGet, h.base, h.adminToken, fmt.Sprintf("/api/v1/models/%d", model.ID), nil)
		if response.StatusCode != http.StatusOK {
			return presence{}
		}
		var result struct {
			Nodes []presence `json:"nodes"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		for _, row := range result.Nodes {
			if row.StorageID == targetStorage {
				return row
			}
		}
		return presence{}
	}
	wait := func(want func(presence) bool) presence {
		timer := time.NewTimer(60 * time.Second)
		defer timer.Stop()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			row := read()
			if want(row) {
				return row
			}
			select {
			case <-ticker.C:
			case <-timer.C:
				t.Fatalf("copy did not reach expected state: %+v", row)
			}
		}
	}
	partial := wait(func(row presence) bool {
		return row.State == "downloading" && row.Downloaded > 1<<20 && row.Downloaded < row.Size
	})
	if _, err := os.Stat(filepath.Join(partial.Path, "model.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("progress appeared only after whole file completed: %v", err)
	}
	killProcess(t, h.server)
	h.server = startStreamServer(t, h)
	h.adminToken = login(t, h.base)
	waitNode(t, h.base, h.adminToken, func(node processNode) bool { return node.UUID == h.node.UUID && node.State == "online" })
	ready := wait(func(row presence) bool { return row.State == "present" })
	actual, err := os.ReadFile(filepath.Join(ready.Path, "model.safetensors"))
	if err != nil || !bytes.Equal(actual, weights) {
		t.Fatalf("resumed contents differ: bytes=%d err=%v", len(actual), err)
	}
	t.Logf("partial=%d/%d bytes, restarted server, verified complete copy=%d bytes", partial.Downloaded, partial.Size, len(actual))
}
