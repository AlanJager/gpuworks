package hub

import (
	"bytes"
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPullImageFromURLForceRefetchesPresentArchive(t *testing.T) {
	h := newArchiveURLHub(t, "file:hub_image_force_url_test?mode=memory&cache=shared", nil)
	if _, err := h.store.UpsertImageNode(&store.ImageNode{
		ImageVersionID: h.image.ID, ImageID: h.image.ID, NodeID: h.node.ID,
		State: store.ImageStatePresent, Source: imagecatalog.FetchSourceURL, ImageDigestID: "sha256:old",
	}); err != nil {
		t.Fatal(err)
	}
	fetch := h.fetch()
	fetch.Force = true
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateSyncing || row.TransferID == "" {
		t.Fatalf("forced archive fetch returned the old present image: %+v", row)
	}
	select {
	case p := <-h.requests:
		if p.TransferID != row.TransferID || p.ArchiveURL != h.url || p.CanonicalRef != h.image.ImageRef {
			t.Fatalf("archive request = %+v, row = %+v", p, row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced archive fetch never reached the node")
	}
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil || again.TransferID != row.TransferID || len(h.requests) != 0 {
		t.Fatalf("forced fetch duplicated an active transfer: row=%+v err=%v requests=%d", again, err, len(h.requests))
	}
	if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{
		ImageRef: h.image.ImageRef, State: store.ImageStatePresent, TransferID: row.TransferID, ImageID: "sha256:new",
	}); err != nil {
		t.Fatal(err)
	}
	final, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || final.State != store.ImageStatePresent || final.ImageDigestID != "sha256:new" || final.TransferID != "" {
		t.Fatalf("forced archive completion = %+v, err=%v", final, err)
	}
}

func TestPullImageFromURLRelayForceRefetchesPresentArchive(t *testing.T) {
	h := newRelayHub(t, "file:hub_image_force_url_relay_test?mode=memory&cache=shared", "/image.tar")
	if _, err := h.store.UpsertImageNode(&store.ImageNode{
		ImageVersionID: h.image.ID, ImageID: h.image.ID, NodeID: h.node.ID,
		State: store.ImageStatePresent, Source: imagecatalog.FetchSourceURLRelay, ImageDigestID: "sha256:old",
	}); err != nil {
		t.Fatal(err)
	}
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceURL, ArchiveURL: h.url, Relay: true, Force: true}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != store.ImageStateSyncing || row.TransferID == "" || row.Source != imagecatalog.FetchSourceURLRelay {
		t.Fatalf("forced relay returned the old present image: %+v", row)
	}
	select {
	case body := <-h.loaded:
		if !bytes.Equal(body, h.archive) {
			t.Fatalf("forced relay loaded %d bytes, want the new archive (%d bytes)", len(body), len(h.archive))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced relay did not download and transfer the archive")
	}
	select {
	case header := <-h.headers:
		if header.Ref != row.TransferID || header.Dst != h.image.ImageRef {
			t.Fatalf("forced relay stream header = %+v, row = %+v", header, row)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced relay did not open a load stream")
	}
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil || again.TransferID != row.TransferID {
		t.Fatalf("forced relay duplicated an active transfer: row=%+v err=%v", again, err)
	}
	if err := h.store.ApplyImageNodeStatus(h.image.ID, h.node.ID, proto.ImageNodeStatus{
		ImageRef: h.image.ImageRef, State: store.ImageStatePresent, TransferID: row.TransferID, ImageID: "sha256:new",
	}); err != nil {
		t.Fatal(err)
	}
	final, err := h.store.ImageNode(h.image.ID, h.node.ID)
	if err != nil || final.State != store.ImageStatePresent || final.ImageDigestID != "sha256:new" || final.TransferID != "" {
		t.Fatalf("forced relay completion = %+v, err=%v", final, err)
	}
	h.assertNothingStaged(t)
}
