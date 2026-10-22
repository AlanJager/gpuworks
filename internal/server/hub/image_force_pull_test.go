package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A rolling tag moves upstream while the node keeps its copy. A plain request
// for an image the node already holds is answered from the row without asking
// the node; a forced one pulls again under a new transfer, and the digest the
// node then reports replaces the old one. Force never starts a second transfer
// while one is still running.
func TestPullImageForceRepullsAPresentImageOnly(t *testing.T) {
	var mu sync.Mutex
	var pulls []proto.ImagePullParams
	h := newArchiveURLHub(t, "file:hub_image_force_pull_test?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string                `json:"method"`
			Params proto.ImagePullParams `json:"params"`
		}
		if json.Unmarshal(env.D, &req) == nil && req.Method == proto.RPCImagePull {
			mu.Lock()
			pulls = append(pulls, req.Params)
			mu.Unlock()
		}
		return `{"result":{"state":"pulling"}}`
	})
	pullCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(pulls)
	}
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageVersionID: h.image.ID, ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStatePresent, Source: "quay"}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{ImageRef: h.image.ImageRef, State: store.ImageStatePresent, ImageID: "sha256:old"}); err != nil {
		t.Fatal(err)
	}
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.SourceDefault}

	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if row.State != store.ImageStatePresent || pullCount() != 0 {
		t.Fatalf("plain request on a present image: row=%+v pulls=%d", row, pullCount())
	}

	fetch.Force = true
	row, err = h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStatePulling || row.TransferID == "" || pullCount() != 1 {
		t.Fatalf("forced pull: row=%+v pulls=%d", row, pullCount())
	}
	mu.Lock()
	first := pulls[0]
	mu.Unlock()
	if first.CanonicalRef != h.image.ImageRef || first.TransferID != row.TransferID || first.EffectiveRef != "quay.io/team/runner:v1" {
		t.Fatalf("pull params = %+v", first)
	}

	// While that transfer runs, a second forced request does not start another.
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if again.TransferID != row.TransferID || pullCount() != 1 {
		t.Fatalf("forced request during a transfer: row=%+v pulls=%d", again, pullCount())
	}

	if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{ImageRef: h.image.ImageRef, State: store.ImageStatePresent, ImageID: "sha256:new", TransferID: row.TransferID}); err != nil {
		t.Fatal(err)
	}
	final, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != store.ImageStatePresent || final.ImageDigestID != "sha256:new" || final.TransferID != "" {
		t.Fatalf("after the re-pull: %+v", final)
	}
}
