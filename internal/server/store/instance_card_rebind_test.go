package store

import (
	"encoding/json"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func rebindFixture(t *testing.T) (*Store, uint) {
	t.Helper()
	s := newTestStore(t)
	node, err := s.RegisterNode("uuid-slurm", HashToken("t"), "slurm-host")
	if err != nil {
		t.Fatal(err)
	}
	ok := true
	gpus := []proto.GPUInfo{card(0, "0000:00:07.0"), card(1, "0000:00:08.0")}
	if err := s.UpdateNodeStatus("uuid-slurm", proto.NodeStatus{GPUProbeOK: &ok, GPUs: gpus}); err != nil {
		t.Fatal(err)
	}
	return s, node.ID
}

// On a Slurm node the agent learns which card an instance got only after
// asking: the row, and the desired state the agent is replayed on every
// reconnect, follow the card it reports, so the ledger counts the card that
// is really in use.
func TestStatusReportRebindsTheInstanceToTheCardsItGot(t *testing.T) {
	s, nodeID := rebindFixture(t)
	instanceRow(t, s, "inst-1", nodeID, []int{0}, []string{"pci:0000:00:07.0"})
	err := s.UpdateInstanceStatusFromNode(nodeID, proto.InstanceStatus{
		UUID: "inst-1", State: proto.InstPending, Phase: proto.PhaseContainerPrepare,
		Message: "the node's allocator gave cards pci:0000:00:08.0", GPUIdents: []string{"pci:0000:00:08.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := instanceByUUID(t, s, "inst-1")
	if got.GPUIndexes != "[1]" || got.GPUIdents != `["pci:0000:00:08.0"]` {
		t.Fatalf("row = %s %s; want device 1, pci:0000:00:08.0", got.GPUIndexes, got.GPUIdents)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(got.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(spec.GPUIndexes, []int{1}) || !slices.Equal(spec.GPUIdents, []string{"pci:0000:00:08.0"}) {
		t.Fatalf("desired state = %v %v; it must agree with the row", spec.GPUIndexes, spec.GPUIdents)
	}
}

// The hub writes progress-only reports in batches that update progress
// columns only; a rebinding must take the full path or it is lost (seen on
// the Aliyun Slurm node: the agent ran on card 0, the row stayed on 1).
func TestARebindingIsNotProgressOnly(t *testing.T) {
	st := proto.InstanceStatus{UUID: "u", State: proto.InstPending, Phase: proto.PhaseContainerPrepare, Message: "m"}
	if !InstanceProgressOnly(st) {
		t.Fatal("fixture is not a progress report")
	}
	st.GPUIdents = []string{"pci:0000:00:08.0"}
	if InstanceProgressOnly(st) {
		t.Fatal("a report carrying a rebinding is treated as progress only")
	}
}

// A report the node's cards cannot back is not applied: the instance would
// be counted on a card nothing knows, or on fewer cards than it holds.
func TestStatusReportIgnoresARebindingTheNodeCannotBack(t *testing.T) {
	s, nodeID := rebindFixture(t)
	instanceRow(t, s, "inst-1", nodeID, []int{0}, []string{"pci:0000:00:07.0"})
	for _, idents := range [][]string{
		{"pci:0000:00:09.0"},
		{"pci:0000:00:07.0", "pci:0000:00:08.0"},
	} {
		if err := s.UpdateInstanceStatusFromNode(nodeID, proto.InstanceStatus{UUID: "inst-1", State: proto.InstPending, GPUIdents: idents}); err != nil {
			t.Fatal(err)
		}
		got := instanceByUUID(t, s, "inst-1")
		if got.GPUIndexes != "[0]" || got.GPUIdents != `["pci:0000:00:07.0"]` {
			t.Fatalf("report %v rebound the row to %s %s", idents, got.GPUIndexes, got.GPUIdents)
		}
	}
}

// The status field is part of the agent's wire format.
func TestInstanceStatusGPUIdentsWire(t *testing.T) {
	raw, err := json.Marshal(proto.InstanceStatus{UUID: "u", GPUIdents: []string{"pci:0000:00:08.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"uuid":"u","state":"","gpu_idents":["pci:0000:00:08.0"]}` {
		t.Fatalf("wire = %s", raw)
	}
	raw, _ = json.Marshal(proto.InstanceStatus{UUID: "u"})
	if string(raw) != `{"uuid":"u","state":""}` {
		t.Fatalf("a status without a rebinding carries the field: %s", raw)
	}
}
