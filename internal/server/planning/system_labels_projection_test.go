package planning

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The column-projected read requirement labels count from yields the same
// system labels as the full node read.
func TestNodesForLabelsYieldTheSameSystemLabels(t *testing.T) {
	st, err := store.Open("file:planning_labels_projection?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i, vendor := range []string{"nvidia", "ascend"} {
		uuid := []string{"proj-a", "proj-b"}[i]
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid, Arch: "arm64", GPUVendor: vendor, Framework: "cann"},
			GPUs: []proto.GPUInfo{{Index: 0, UUID: uuid + "-0", Name: "NVIDIA A100-SXM4-80GB", Vendor: vendor, ArchID: "ascend910b", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true}}}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "inference"}); err != nil {
			t.Fatal(err)
		}
	}
	full, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	projected, err := st.NodesForLabels(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 2 || len(projected) != 2 {
		t.Fatalf("read %d and %d nodes", len(full), len(projected))
	}
	for i := range full {
		want := SystemLabels(&full[i], NodeFacts{})
		got := SystemLabels(&projected[i], NodeFacts{})
		if len(want) < 4 || !reflect.DeepEqual(got, want) {
			t.Fatalf("node %s: projected labels %+v, full %+v", full[i].UUID, got, want)
		}
		if projected[i].Labels != full[i].Labels {
			t.Fatalf("written labels %q vs %q", projected[i].Labels, full[i].Labels)
		}
	}
}

// Without its cards a node still carries every label but the card ones.
func TestNodesForLabelsWithoutCardsLeavesOnlyCardLabelsOff(t *testing.T) {
	st, err := store.Open("file:planning_labels_projection_nocards?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.RegisterNode("nocards", store.HashToken("nocards"), "nocards"); err != nil {
		t.Fatal(err)
	}
	status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: "nocards", Arch: "amd64", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{{Index: 0, UUID: "nocards-0", Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true}}}
	if err := st.UpdateNodeStatus("nocards", status); err != nil {
		t.Fatal(err)
	}
	full, _ := st.ListNodes()
	bare, err := st.NodesForLabels(false)
	if err != nil || len(bare) != 1 || len(bare[0].GPUs) != 0 {
		t.Fatalf("bare read = %+v, %v", bare, err)
	}
	var want []SystemLabel
	for _, label := range SystemLabels(&full[0], NodeFacts{}) {
		if label.Key != nodelabel.KeyGPUModel && label.Key != nodelabel.KeyGPUArch {
			want = append(want, label)
		}
	}
	if got := SystemLabels(&bare[0], NodeFacts{}); !reflect.DeepEqual(got, want) || len(want) < 3 {
		t.Fatalf("labels without cards = %+v, want %+v", got, want)
	}
	if !RequirementsNeedCards(nodelabel.Requirements{nodelabel.KeyGPUModel: {"A100"}}) || RequirementsNeedCards(nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}}) {
		t.Fatal("RequirementsNeedCards")
	}
}
