package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func imagePullCompletionFixture(t *testing.T, state string) (*Store, *Image, *Node) {
	t.Helper()
	s := newRaceTestStore(t)
	image, err := s.UpsertImage(&Image{ImageRef: "example/pull-completion:latest", Engine: "vllm", Enabled: true, SizeBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	node, err := s.RegisterNode("pull-completion", HashToken("test-token"), "pull-completion")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertImageNode(&ImageNode{
		ImageID: image.ID, NodeID: node.ID, State: state, TransferID: "active",
		SizeBytes: 100, TransferredBytes: 40, RateBPS: 10,
	}); err != nil {
		t.Fatal(err)
	}
	return s, image, node
}

// go test ./internal/server/store -run '^TestImagePullCompletionClearsPartialTotals$' -count=1
func TestImagePullCompletionClearsPartialTotals(t *testing.T) {
	for _, tc := range []struct {
		name, initial, terminal, transferID string
		total, done, wantTotal, wantDone    int64
	}{
		{name: "registry success", initial: ImageStatePulling, terminal: ImageStatePresent, transferID: "active"},
		{name: "recovered registry success", initial: ImageStatePulling, terminal: ImageStatePresent, transferID: "active", total: 100, done: 100},
		{name: "legacy registry success", initial: ImageStatePulling, terminal: ImageStatePresent},
		{name: "failed registry pull", initial: ImageStatePulling, terminal: ImageStateFailed, transferID: "active", wantTotal: 100, wantDone: 40},
		{name: "archive success keeps byte counts", initial: ImageStateSyncing, terminal: ImageStatePresent, transferID: "active", wantTotal: 100, wantDone: 40},
		{name: "archive final byte counts", initial: ImageStateSyncing, terminal: ImageStatePresent, transferID: "active", total: 120, done: 120, wantTotal: 120, wantDone: 120},
		{name: "stale completion", initial: ImageStatePulling, terminal: ImageStatePresent, transferID: "older", wantTotal: 100, wantDone: 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, image, node := imagePullCompletionFixture(t, tc.initial)
			if err := s.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{
				State: tc.terminal, TransferID: tc.transferID, SizeBytes: tc.total, TransferredBytes: tc.done,
			}); err != nil {
				t.Fatal(err)
			}
			row, err := s.ImageNode(image.ID, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantTransfer, wantRate := tc.terminal, "", int64(0)
			if tc.transferID == "older" {
				wantState, wantTransfer, wantRate = tc.initial, "active", 10
			}
			if row.State != wantState || row.TransferID != wantTransfer || row.RateBPS != wantRate || row.SizeBytes != tc.wantTotal || row.TransferredBytes != tc.wantDone {
				t.Fatalf("completion row = %+v; want state=%s transfer=%q rate=%d bytes=%d/%d", row, wantState, wantTransfer, wantRate, tc.wantDone, tc.wantTotal)
			}
			if row.RuntimeSizeBytes != nil {
				t.Fatalf("transfer counters became a runtime measurement: %d", *row.RuntimeSizeBytes)
			}
			if wantState != ImageStatePresent {
				return
			}
			// A later runtime observation must remain independent of transfer
			// and catalog archive sizes, including an unknown registry total.
			runtimeSize := int64(2000)
			if err := s.UpdateNodeStatus(node.UUID, proto.NodeStatus{Images: []proto.ImagePresence{{
				ImageRef: image.ImageRef, Present: true, RuntimeSizeBytes: &runtimeSize,
			}}}); err != nil {
				t.Fatal(err)
			}
			row, err = s.ImageNode(image.ID, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.RuntimeSizeBytes == nil || *row.RuntimeSizeBytes != runtimeSize || row.SizeBytes != tc.wantTotal || row.TransferredBytes != tc.wantDone {
				t.Fatalf("runtime observation changed transfer accounting: %+v", row)
			}
		})
	}
}

// go test ./internal/server/store -run '^TestImagePresenceKeepsRegistryPullActive$' -count=1
func TestImagePresenceKeepsRegistryPullActive(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent"
		if present {
			name = "present"
		}
		t.Run(name, func(t *testing.T) {
			s, image, node := imagePullCompletionFixture(t, ImageStatePulling)
			runtimeSize := int64(2000)
			if err := s.UpdateNodeStatus(node.UUID, proto.NodeStatus{Images: []proto.ImagePresence{{
				ImageRef: image.ImageRef, Present: present, RuntimeSizeBytes: &runtimeSize,
			}}}); err != nil {
				t.Fatal(err)
			}
			row, err := s.ImageNode(image.ID, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != ImageStatePulling || row.TransferID != "active" || row.SizeBytes != 100 || row.TransferredBytes != 40 || row.RuntimeSizeBytes != nil {
				t.Fatalf("inventory completed an active registry pull: %+v", row)
			}
			if err := s.ApplyImageNodeStatus(image.ID, node.ID, proto.ImageNodeStatus{State: ImageStatePresent, TransferID: "active"}); err != nil {
				t.Fatal(err)
			}
			row, err = s.ImageNode(image.ID, node.ID)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != ImageStatePresent || row.SizeBytes != 0 || row.TransferredBytes != 0 {
				t.Fatalf("inventory prevented registry completion cleanup: %+v", row)
			}
		})
	}
}
