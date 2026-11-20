package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A slice's own memory and compute readings come from its partition
// backend's interposer, and not every backend keeps both. GET /dgpus carries
// a reading where a report brought one, says which readings a backend never
// has, and leaves a reading that is merely not in yet as neither - so a
// client can tell "no report yet" from "this backend has none".
func TestDGPUListSaysWhichReadingsABackendKeeps(t *testing.T) {
	st, err := store.Open("file:dgpu_slice_usage_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("node-usage", store.HashToken("t"), "node-usage")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "usage", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "GPU-0", Name: "H20", Vendor: "nvidia", MemoryTotalBytes: 96 << 30, Healthy: true, HealthStatus: "OK"},
			{Index: 1, UUID: "GPU-1", Name: "H20", Vendor: "nvidia", MemoryTotalBytes: 96 << 30, Healthy: true, HealthStatus: "OK"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	node, _ = st.NodeWithGPUs(node.UUID)
	dep, err := st.CreateDeployment(&store.Deployment{Name: "usage", NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	type reading struct {
		memory, sm float64 // what the node's last report carried; -1 for none
	}
	slices := []struct {
		uuid, backend string
		smLimit       int
		reported      reading
		// The readings and flags the row must carry on the wire, by key.
		want map[string]any
	}{
		{"nvidia-slice", accelerator.BackendNVIDIALogical, 0, reading{7 << 30, 42},
			map[string]any{"memory_used_bytes": float64(7 << 30), "sm_util_pct": 42.0, "updated_at": true}},
		// A desired state from before backends were named is NVIDIA's.
		{"legacy-slice", "", 0, reading{-1, -1}, map[string]any{}},
		{"ascend-slice", accelerator.BackendAscendLogical, 0, reading{6860 << 20, -1},
			map[string]any{"memory_used_bytes": float64(6860 << 20), "updated_at": true, "sm_util_unsupported": true}},
		{"ascend-starting", accelerator.BackendAscendLogical, 0, reading{-1, -1}, map[string]any{"sm_util_unsupported": true}},
		{"ppu-capped", accelerator.BackendPPULogical, 50, reading{7168 << 20, 37},
			map[string]any{"memory_used_bytes": float64(7168 << 20), "sm_util_pct": 37.0, "updated_at": true}},
		{"ppu-uncapped", accelerator.BackendPPULogical, 0, reading{7168 << 20, -1},
			map[string]any{"memory_used_bytes": float64(7168 << 20), "updated_at": true, "sm_util_unsupported": true}},
		{"hygon-slice", accelerator.BackendHygonLogical, 0, reading{-1, -1},
			map[string]any{"memory_used_unsupported": true, "sm_util_unsupported": true}},
		{"amd-slice", accelerator.BackendAMDLogical, 0, reading{-1, -1},
			map[string]any{"memory_used_unsupported": true, "sm_util_unsupported": true}},
	}
	now := time.Now().UnixMilli()
	var memory, sm []MetricLatest
	for i, slice := range slices {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: slice.uuid, Engine: "vllm", VRAMLimitBytes: 8 << 30, SMLimitPct: slice.smLimit, PartitionBackend: slice.backend})
		gpu := i % 2
		if _, err := st.CreateInstance(&store.Instance{UUID: slice.uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
			GPUIndexes: map[int]string{0: "[0]", 1: "[1]"}[gpu], DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
		labels := map[string]string{"node": "node-usage", "gpu": map[int]string{0: "0", 1: "1"}[gpu], "instance": slice.uuid}
		if slice.reported.memory >= 0 {
			memory = append(memory, MetricLatest{Labels: labels, Value: slice.reported.memory, TimeMS: now})
		}
		if slice.reported.sm >= 0 {
			sm = append(sm, MetricLatest{Labels: labels, Value: slice.reported.sm, TimeMS: now})
		}
	}
	h := apiHandlers{d: Deps{Store: st, MetricsLatest: func(metric string, _ map[string]string, _ int64, _ ...int64) ([]MetricLatest, error) {
		switch metric {
		case "gpuworks_dgpu_memory_used_bytes":
			return memory, nil
		case "gpuworks_dgpu_sm_util":
			return sm, nil
		}
		return nil, nil
	}}}
	rec := httptest.NewRecorder()
	h.listDGPUs(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dgpus", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, item := range body.Items {
		rows[item["instance_uuid"].(string)] = item
	}
	if len(rows) != len(slices) {
		t.Fatalf("rows = %d, want %d: %s", len(rows), len(slices), rec.Body.String())
	}
	for _, slice := range slices {
		got := map[string]any{}
		for _, key := range []string{"memory_used_bytes", "sm_util_pct", "updated_at", "memory_used_unsupported", "sm_util_unsupported"} {
			value, present := rows[slice.uuid][key]
			if !present {
				continue
			}
			if key == "updated_at" {
				stamp, err := time.Parse(time.RFC3339Nano, value.(string))
				if err != nil || stamp.UnixMilli() != now {
					t.Fatalf("%s updated_at = %v (%v), want the report's time", slice.uuid, value, err)
				}
				value = true
			}
			got[key] = value
		}
		if !reflect.DeepEqual(got, slice.want) {
			t.Errorf("%s readings on the wire = %v, want %v", slice.uuid, got, slice.want)
		}
	}
}
