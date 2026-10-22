//go:build multiproc

package l2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
	"gpuworks.ai/gpuworks/test/ocifixture"
)

// peerSyncCluster is one real server and several real agents on the recording
// runtime. Like an ordinary agent_dial install, no agent sets a direct
// address: the target advertises its own interface addresses and the
// source-to-target TLS connection is a real socket between two agent
// processes that the server only coordinates.
type peerSyncCluster struct {
	base, token string
	nodes       map[string]processNode
	agents      map[string]*exec.Cmd
}

func startPeerSyncCluster(t *testing.T, absentImage string, names ...string) *peerSyncCluster {
	t.Helper()
	ensureEmbeddedAgentFor(t, runtime.GOARCH)
	serverBin := buildBinary(t, "./cmd/gpuworks")
	agentBin := buildBinary(t, "./cmd/gpuworks-agent")
	port := freePort(t)
	base := "http://127.0.0.1:" + port
	server := serverCommand(t, "multiproc-password", serverBin, "serve", "--addr", "127.0.0.1:"+port, "--data", t.TempDir())
	server.Stdout, server.Stderr = io.Discard, io.Discard
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcess(server) })
	waitHTTP(t, base+"/api/v1/healthz", serverHealthTimeout, func(r *http.Response) bool { return r.StatusCode == http.StatusUnauthorized })
	c := &peerSyncCluster{base: base, token: login(t, base), nodes: map[string]processNode{}, agents: map[string]*exec.Cmd{}}
	for _, name := range names {
		seed, nodeToken := createNode(t, base, c.token, name)
		agent := exec.Command(agentBin, "run", "--server", base, "--token", nodeToken, "--state", filepath.Join(t.TempDir(), name))
		agent.Env = append(os.Environ(),
			"GPUWORKS_FAKE_GPU=A100x1,81920",
			"GPUWORKS_FAKE_RUNTIME_RECORD="+filepath.Join(t.TempDir(), name+".json"),
			"GPUWORKS_FAKE_RUNTIME_ABSENT_IMAGES="+absentImage,
			"GPUWORKS_LOG_LEVEL=warn",
		)
		agent.Stdout, agent.Stderr = io.Discard, io.Discard
		if err := agent.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stopProcess(agent) })
		c.agents[name] = agent
		c.nodes[name] = waitNode(t, base, c.token, func(n processNode) bool { return n.UUID == seed.UUID && n.State == "online" })
	}
	return c
}

func (c *peerSyncCluster) post(t *testing.T, path string, body any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, data := authedDo(t, http.MethodPost, c.base, c.token, path, raw)
	return resp.StatusCode, string(data)
}

func (c *peerSyncCluster) probe(t *testing.T, path, source string) {
	t.Helper()
	if status, body := c.post(t, path, map[string]string{"source_node": source}); status != http.StatusOK {
		t.Fatalf("probe %s status=%d body=%s", path, status, body)
	}
}

type peerImageRow struct {
	NodeUUID         string `json:"node_uuid"`
	State            string `json:"state"`
	Source           string `json:"source"`
	SourceNodeUUID   string `json:"source_node_uuid"`
	RateLimitKiB     int64  `json:"rate_limit_kib_s"`
	LastRateLimitKiB int64  `json:"last_rate_limit_kib_s"`
	Message          string `json:"message"`
}

// imageRow reads one node's row for imageRef. The catalog list names the
// version's series; the series detail carries the node rows.
func (c *peerSyncCluster) imageRow(t *testing.T, imageRef, nodeUUID string) peerImageRow {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, c.base, c.token, "/api/v1/images/versions", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list image versions status=%d body=%s", resp.StatusCode, data)
	}
	var listed []struct {
		SeriesID uint   `json:"series_id"`
		ImageRef string `json:"image_ref"`
	}
	if err := apiwire.UnmarshalList(data, &listed); err != nil {
		t.Fatal(err)
	}
	for _, version := range listed {
		if version.ImageRef != imageRef {
			continue
		}
		resp, data := authedDo(t, http.MethodGet, c.base, c.token, fmt.Sprintf("/api/v1/images/%d", version.SeriesID), nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("image series status=%d body=%s", resp.StatusCode, data)
		}
		var series struct {
			Versions []struct {
				ImageRef string         `json:"image_ref"`
				Nodes    []peerImageRow `json:"nodes"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(data, &series); err != nil {
			t.Fatal(err)
		}
		for _, detailed := range series.Versions {
			for _, row := range detailed.Nodes {
				if detailed.ImageRef == imageRef && row.NodeUUID == nodeUUID {
					return row
				}
			}
		}
	}
	return peerImageRow{NodeUUID: nodeUUID, State: "absent"}
}

func (c *peerSyncCluster) waitImageTerminal(t *testing.T, imageRef, nodeUUID string) peerImageRow {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		row := c.imageRow(t, imageRef, nodeUUID)
		if row.State == "present" || row.State == "failed" || time.Now().After(deadline) {
			return row
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestImagePeerSyncDirect is l2-image-peer-sync-direct: the image is seeded on
// one node from an HTTP archive, then copied agent-to-agent after a real probe.
// Failure cases: no source is refused, and an offline source is refused
// without falling back to the archive site or another node.
func TestImagePeerSyncDirect(t *testing.T) {
	const imageRef = "gpustack/l2-peer-runner:cuda13.0-vllm0.25.1"
	c := startPeerSyncCluster(t, imageRef, "peer-src", "peer-dst", "peer-dst2")
	src, dst, dst2 := c.nodes["peer-src"].UUID, c.nodes["peer-dst"].UUID, c.nodes["peer-dst2"].UUID

	archive, err := ocifixture.DockerSaveArchive(ocifixture.DockerSaveOptions{RepoTags: []string{imageRef}, LayerBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	site, siteServer := ocifixture.StartArchiveServer(archive)
	t.Cleanup(siteServer.Close)
	archiveURL := siteServer.URL + "/image.tar"
	if status, body := c.post(t, "/api/v1/images/import", map[string]any{"series": []map[string]any{{
		"name": "l2-peer-runner", "engine": "vllm", "purpose": "inference", "source": "custom",
		"versions": []map[string]any{{"image_ref": imageRef, "source": "custom", "arch": "multi", "command_mode": "manual", "registry_sources": []string{archiveURL}}},
	}}}); status != http.StatusOK && status != http.StatusCreated {
		t.Fatalf("import status=%d body=%s", status, body)
	}
	version := findArchiveImportVersion(t, c.base, c.token, imageRef)
	syncPath := func(node string) string { return fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", version.ID, node) }

	// Seed the source from the archive site.
	if status, body := c.post(t, syncPath(src), map[string]any{"source": "url", "archive_url": archiveURL}); status != http.StatusAccepted {
		t.Fatalf("seed status=%d body=%s", status, body)
	}
	if row := c.waitImageTerminal(t, imageRef, src); row.State != "present" {
		t.Fatalf("seed row = %+v", row)
	}
	siteGets := func() int {
		n := 0
		for _, request := range site.Requests() {
			if strings.HasPrefix(request, "GET /image.tar") && !strings.Contains(request, "bytes=") {
				n++
			}
		}
		return n
	}
	seededGets := siteGets()

	// Failure: naming a registry is refused (pulls follow the pull source
	// rules) and starts nothing.
	if status, body := c.post(t, syncPath(dst), map[string]any{"source": "quay"}); status != http.StatusBadRequest || !strings.Contains(body, "IMAGE_SOURCE_REMOVED") {
		t.Fatalf("sync naming a registry status=%d body=%s", status, body)
	}

	// Success: probe, then a rate-limited direct transfer.
	c.probe(t, syncPath(dst)+"/probe", src)
	if status, body := c.post(t, syncPath(dst), map[string]any{"source_node": src, "rate_limit_kib_s": 1024}); status != http.StatusAccepted {
		t.Fatalf("peer sync status=%d body=%s", status, body)
	}
	row := c.waitImageTerminal(t, imageRef, dst)
	if row.State != "present" || row.Source != "peer" || row.SourceNodeUUID != src {
		t.Fatalf("peer row = %+v, want present from %s", row, src)
	}
	if row.RateLimitKiB != 0 || row.LastRateLimitKiB != 1024 {
		t.Fatalf("rate limit after completion = active %d last %d, want 0 and 1024", row.RateLimitKiB, row.LastRateLimitKiB)
	}
	if got := siteGets(); got != seededGets {
		t.Fatalf("archive site GETs went from %d to %d: the peer copy did not come from the source node", seededGets, got)
	}

	// Failure: with the source offline, probe and sync are refused, and the
	// target is not served from the site or from the node that now has it.
	stopProcess(c.agents["peer-src"])
	waitNode(t, c.base, c.token, func(n processNode) bool { return n.UUID == src && n.State != "online" })
	if status, body := c.post(t, syncPath(dst2)+"/probe", map[string]string{"source_node": src}); status != http.StatusConflict {
		t.Fatalf("probe with offline source status=%d body=%s", status, body)
	}
	if status, body := c.post(t, syncPath(dst2), map[string]any{"source_node": src}); status != http.StatusConflict {
		t.Fatalf("sync with offline source status=%d body=%s", status, body)
	}
	time.Sleep(time.Second)
	if row := c.imageRow(t, imageRef, dst2); row.State == "present" || row.State == "syncing" {
		t.Fatalf("offline source fell back to another path: %+v", row)
	}
	if got := siteGets(); got != seededGets {
		t.Fatalf("offline source fell back to the archive site: GETs %d -> %d", seededGets, got)
	}
}

// TestModelPeerSyncDirect is l2-model-peer-sync-direct: a model directory
// registered on one node is copied agent-to-agent after a real probe. Failure
// case: with the source offline, an explicit sync and a body-less retry both
// keep the recorded source and stop at download_required instead of falling
// back to a public download.
func TestModelPeerSyncDirect(t *testing.T) {
	c := startPeerSyncCluster(t, "", "model-src", "model-dst", "model-dst2")
	src, dst, dst2 := c.nodes["model-src"].UUID, c.nodes["model-dst"].UUID, c.nodes["model-dst2"].UUID

	// browser_metadata keeps registration offline: no Hugging Face request.
	resp, data := authedDo(t, http.MethodPost, c.base, c.token, "/api/v1/models", mustJSON(t, map[string]any{
		"source": "huggingface", "source_id": "l2/peer-model", "revision": "main", "browser_metadata": true,
		"config_raw":   `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
		"weight_bytes": 4096,
	}))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create model status=%d body=%s", resp.StatusCode, data)
	}
	var model struct {
		ID uint `json:"id"`
	}
	if err := json.Unmarshal(data, &model); err != nil || model.ID == 0 {
		t.Fatalf("create model body=%s err=%v", data, err)
	}
	sourceDir := t.TempDir()
	weights := bytes.Repeat([]byte("l2-peer-weight-"), 256)
	if err := os.WriteFile(filepath.Join(sourceDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "model.safetensors"), weights, 0o644); err != nil {
		t.Fatal(err)
	}
	if status, body := c.post(t, fmt.Sprintf("/api/v1/models/%d/register-path", model.ID), map[string]string{"node_uuid": src, "path": sourceDir}); status != http.StatusOK {
		t.Fatalf("register-path status=%d body=%s", status, body)
	}
	syncPath := func(node string) string { return fmt.Sprintf("/api/v1/models/%d/nodes/%s/sync", model.ID, node) }

	// Success: probe, then the direct copy lands byte-for-byte on the target.
	c.probe(t, syncPath(dst)+"/probe", src)
	if status, body := c.post(t, syncPath(dst), map[string]any{"source_node": src, "rate_limit_kib_s": 0}); status != http.StatusAccepted {
		t.Fatalf("peer sync status=%d body=%s", status, body)
	}
	waitModelPresence(t, c.base, c.token, model.ID, dst, 60*time.Second)
	presence := c.modelPresence(t, model.ID, dst)
	if presence.Acquisition != "peer" {
		t.Fatalf("target presence = %+v, want peer acquisition", presence)
	}
	if got, err := os.ReadFile(filepath.Join(presence.Path, "model.safetensors")); err != nil || !bytes.Equal(got, weights) {
		t.Fatalf("target weights err=%v bytes=%d want %d", err, len(got), len(weights))
	}

	// Failure: the selected source goes offline. The explicit sync records the
	// source and stops; the body-less retry the UI sends keeps that source.
	stopProcess(c.agents["model-src"])
	waitNode(t, c.base, c.token, func(n processNode) bool { return n.UUID == src && n.State != "online" })
	for _, body := range []any{map[string]any{"source_node": src, "rate_limit_kib_s": 2048}, map[string]any{}} {
		if status, raw := c.post(t, syncPath(dst2), body); status != http.StatusAccepted {
			t.Fatalf("sync %v with offline source status=%d body=%s", body, status, raw)
		}
		got := c.modelPresence(t, model.ID, dst2)
		if got.State != "download_required" || got.Acquisition != "peer" {
			t.Fatalf("offline source presence = %+v, want download_required via peer", got)
		}
	}
}

type peerModelPresence struct {
	NodeUUID    string `json:"node_uuid"`
	State       string `json:"state"`
	Path        string `json:"path"`
	Acquisition string `json:"acquisition"`
	Message     string `json:"message"`
}

func (c *peerSyncCluster) modelPresence(t *testing.T, modelID uint, nodeUUID string) peerModelPresence {
	t.Helper()
	resp, data := authedDo(t, http.MethodGet, c.base, c.token, fmt.Sprintf("/api/v1/models/%d", modelID), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get model status=%d body=%s", resp.StatusCode, data)
	}
	var model struct {
		Nodes []peerModelPresence `json:"nodes"`
	}
	if err := json.Unmarshal(data, &model); err != nil {
		t.Fatal(err)
	}
	for _, presence := range model.Nodes {
		if presence.NodeUUID == nodeUUID {
			return presence
		}
	}
	t.Fatalf("model %d has no presence on %s: %s", modelID, nodeUUID, data)
	return peerModelPresence{}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
