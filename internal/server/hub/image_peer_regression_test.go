package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPeerProbeRejectsExistingTransfer(t *testing.T) {
	call := func(_ context.Context, _ string, method string, _ any, _ bool) (json.RawMessage, error) {
		if method != proto.RPCImageSyncListen && method != proto.RPCModelSyncListen {
			t.Fatalf("unexpected RPC %s", method)
		}
		return json.RawMessage(`{"in_progress":true,"transfer_id":"old"}`), nil
	}
	_, err := coordinateImageCopy(context.Background(), proto.ImageSyncCopyRequest{ImageRef: "example:test", SourceNodeUUID: "src", TargetNodeUUID: "dst"}, "token", call, true)
	if err == nil {
		t.Fatal("image probe claimed success without handshake")
	}
	_, err = coordinateModelCopyProbe(context.Background(), proto.ModelSyncCopyRequest{Key: "key", SourceNodeUUID: "src", TargetNodeUUID: "dst"}, "token", call)
	if err == nil {
		t.Fatal("model probe claimed success without handshake")
	}
}

func TestImageReconnectRetainsRecentTransfer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.RegisterNode("src", store.HashToken("src"), "src")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := st.RegisterNode("dst", store.HashToken("dst"), "dst")
	if err != nil {
		t.Fatal(err)
	}
	img, err := st.UpsertImage(&store.Image{ImageRef: "example:recent", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.UpsertImageNode(&store.ImageNode{ImageID: img.ID, NodeID: dst.ID, SourceNodeID: src.ID, State: store.ImageStateSyncing, TransferID: "original", RateLimitKiB: 10240})
	if err != nil {
		t.Fatal(err)
	}
	h := New(st, slog.Default())
	h.sessions[dst.UUID] = &Session{}
	for i := 0; i < 2; i++ {
		got, err := h.syncImageDirect(context.Background(), img.ID, dst.UUID, src.UUID, 10240, true)
		if err != nil {
			t.Fatal(err)
		}
		if got.TransferID != "original" || got.RateLimitKiB != 10240 {
			t.Fatalf("reconnect replaced active transfer: %+v", got)
		}
	}
	if err := st.ApplyImageNodeStatus(img.ID, dst.ID, proto.ImageNodeStatus{ImageRef: img.ImageRef, State: store.ImageStatePresent, TransferID: "original"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ImageNode(img.ID, dst.ID)
	if err != nil || got.State != store.ImageStatePresent {
		t.Fatalf("original completion rejected: %+v %v", got, err)
	}
}
