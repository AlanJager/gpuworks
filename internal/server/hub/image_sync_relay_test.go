package hub

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A copy started without the relay never asks for it, whatever the two nodes
// allow; a copy that may be relayed still needs both nodes to let the
// management network carry storage sync.
func TestImageCopyRequestAsksForTheRelayOnlyWhenTheCopyMayUseIt(t *testing.T) {
	image := &store.Image{ImageRef: "fixture/image:relay"}
	open := &store.Node{UUID: "open"}
	closed := &store.Node{UUID: "closed", ManagementRolesOff: `["` + netplan.RoleStorageSync + `"]`}
	path := netplan.StoragePath{Network: netplan.ManagementName, SourceIP: "10.0.0.1", TargetIP: "10.0.0.2"}
	for _, tc := range []struct {
		name           string
		source, target *store.Node
		relay, want    bool
	}{
		{"direct only", open, open, false, false},
		{"relay allowed by both nodes", open, open, true, true},
		{"relay refused by the target", open, closed, true, false},
		{"relay refused by the source", closed, open, true, false},
	} {
		req := imageCopyRequest(image, tc.source, tc.target, path, "transfer", 1024, 512, tc.relay)
		if req.AllowRelay != tc.want {
			t.Errorf("%s: AllowRelay = %v, want %v", tc.name, req.AllowRelay, tc.want)
		}
		if req.SourceIP != "10.0.0.1" || req.TargetIP != "10.0.0.2" || req.RateLimitKBs != 512 || req.ExpectedBytes != 1024 || req.ImageRef != image.ImageRef {
			t.Errorf("%s: request = %+v", tc.name, req)
		}
	}
}

// The copy a launch starts is recorded like any other peer copy: the same
// ledger row, the same source, so the launch can wait on it and cool the pair
// when it fails.
func TestSyncImageDirectFromRecordsThePeerCopy(t *testing.T) {
	h, image, source, target := imageStallFixture(t, "direct_only_row")
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: image.ID, NodeID: source.ID, State: store.ImageStatePresent, SizeBytes: 2048}); err != nil {
		t.Fatal(err)
	}
	h.sessions[target.UUID] = &Session{}
	h.sessions[source.UUID] = &Session{}
	row, err := h.SyncImageDirectFrom(context.Background(), image.ID, target.UUID, source.UUID, 256)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateSyncing || row.Source != "peer" || row.SourceNodeID != source.ID || row.RateLimitKiB != 256 {
		t.Fatalf("row = %+v, want a syncing peer copy from the source at the given cap", row)
	}
	if _, err := h.SyncImageDirectFrom(context.Background(), image.ID, target.UUID, "", 0); err == nil {
		t.Fatal("a copy with no source was started")
	}
}
