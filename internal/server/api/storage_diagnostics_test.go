package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageProbeDiagnosticProvenance(t *testing.T) {
	for _, ok := range []bool{true, false} {
		t.Run(map[bool]string{true: "success", false: "failed"}[ok], func(t *testing.T) {
			st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("probe-node", store.HashToken("node-token"), "probe-host")
			if err != nil {
				t.Fatal(err)
			}
			h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
				if uuid != node.UUID || method != proto.RPCStorageProbe {
					t.Fatalf("wrong probe %s %s", uuid, method)
				}
				p := params.(proto.StorageProbeParams)
				if p.Export != "nfs.example:/models" {
					t.Fatalf("target %+v", p)
				}
				return json.Marshal(proto.StorageProbeResult{OK: ok, Readable: ok, Writable: ok, Reason: map[bool]string{true: "", false: "mount_failed"}[ok]})
			}}}
			before := time.Now()
			rec := httptest.NewRecorder()
			h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(`{"name":"models","type":"nfs","export":"nfs.example:/models","node_uuid":"`+node.UUID+`"}`)))
			after := time.Now()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d %s", rec.Code, rec.Body)
			}
			var wire map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			if wire["target"] != "nfs.example:/models" || wire["node_uuid"] != node.UUID || wire["ok"] != ok {
				t.Fatalf("provenance %v", wire)
			}
			stamp, _ := wire["checked_at"].(string)
			at, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil || at.Before(before) || at.After(after) {
				t.Fatalf("result receipt timestamp %q outside %s..%s", stamp, before, after)
			}
		})
	}
}

func TestStorageProbeRPCFailureHasNoCompletedObservation(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("n", store.HashToken("node-token"), "host")
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
		return nil, errors.New("node RPC disconnected")
	}}}
	rec := httptest.NewRecorder()
	h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(`{"type":"local","path":"/data/models","node_uuid":"`+node.UUID+`"}`)))
	if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), `"checked_at"`) {
		t.Fatalf("RPC failure claimed completed node probe: %d %s", rec.Code, rec.Body)
	}
}

func TestStorageProbeDiagnosticTargetMatchesAgentBranch(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		existing   bool
		target     string
	}{
		{"new_nfs", proto.StorageNFS, false, "nfs.example:/models"},
		{"new_nfs_cache", proto.StorageNFSCache, false, "nfs.example:/models"},
		{"existing_nfs", proto.StorageNFS, true, "/data/models"},
		{"existing_nfs_cache", proto.StorageNFSCache, true, "/data/models"},
		{"local_ignores_export", proto.StorageLocal, false, "/data/models"},
		{"shared_dir_ignores_export", proto.StorageSharedDir, false, "/data/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("probe-node", store.HashToken("node-token"), "probe-host")
			if err != nil {
				t.Fatal(err)
			}
			called := false
			h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
				called = true
				p := params.(proto.StorageProbeParams)
				if uuid != node.UUID || method != proto.RPCStorageProbe || p.Type != tc.kind || p.Path != "/data/models" || p.Export != "nfs.example:/models" || p.Existing != tc.existing {
					t.Fatalf("actual agent request=%+v node=%s method=%s", p, uuid, method)
				}
				return json.Marshal(proto.StorageProbeResult{OK: true, Readable: true, Writable: true})
			}}}
			body, _ := json.Marshal(map[string]any{"node_uuid": node.UUID, "type": tc.kind, "path": "/data/models", "export": "nfs.example:/models", "existing": tc.existing})
			rec := httptest.NewRecorder()
			h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(string(body))))
			var wire map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &wire)
			if rec.Code != http.StatusOK || !called || wire["target"] != tc.target {
				t.Fatalf("type=%s existing=%v result=%d %s; want actual branch target %q", tc.kind, tc.existing, rec.Code, rec.Body, tc.target)
			}
		})
	}
}
