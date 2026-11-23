package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImageLoadPathDispatchesToTargetWithoutReadingServerFile(t *testing.T) {
	requests := make(chan proto.ImageLoadPathParams, 1)
	h := newArchiveURLHub(t, "file:hub_local_image?mode=memory&cache=shared", func(env proto.Envelope) string {
		var req struct {
			Method string                    `json:"method"`
			Params proto.ImageLoadPathParams `json:"params"`
		}
		_ = json.Unmarshal(env.D, &req)
		if req.Method != proto.RPCImageLoadPath {
			return `{"error":"unexpected RPC"}`
		}
		requests <- req.Params
		return `{"result":{"state":"syncing"}}`
	})
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceLocal, ArchivePath: "/only/on/target/image.tar"}
	row, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil {
		t.Fatal(err)
	}
	got := <-requests
	if got.ArchivePath != fetch.ArchivePath || got.CanonicalRef != h.image.ImageRef || got.TransferID == "" || got.TransferID != row.TransferID || row.Source != imagecatalog.FetchSourceLocal {
		t.Fatalf("dispatch = %+v, row = %+v", got, row)
	}
	// A duplicate click must leave the existing attempt in charge.
	again, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, fetch)
	if err != nil || again.TransferID != row.TransferID || len(requests) != 0 {
		t.Fatalf("duplicate request = %+v, %v", again, err)
	}
}

func TestImageLoadPathOldAgentLeavesRetryableFailure(t *testing.T) {
	for name, reply := range map[string]string{
		"legacy": `{"error":"unknown rpc method: image.load_path"}`,
		"coded":  `{"error":"method unavailable","code":"RPC_UNKNOWN_METHOD"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newArchiveURLHub(t, "file:"+t.Name()+"?mode=memory&cache=shared", func(proto.Envelope) string { return reply })
			_, err := h.hub.PullImage(context.Background(), h.image.ID, h.node.UUID, imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceLocal, ArchivePath: "/data/image.tar"})
			if err == nil || !strings.Contains(err.Error(), "upgrade the agent") {
				t.Fatalf("expected upgrade message, got %v", err)
			}
			row, err := h.store.ImageNode(h.image.ID, h.node.ID)
			if err != nil || row.State != store.ImageStateFailed || row.Source != imagecatalog.FetchSourceLocal {
				t.Fatalf("failed import = %+v, %v", row, err)
			}
		})
	}
}

func TestLocalArchiveReplacementRequiresForceAndKeepsActiveTransfer(t *testing.T) {
	calls := make(chan proto.Envelope, 2)
	h := newArchiveURLHub(t, "file:replace_local_image?mode=memory&cache=shared", func(env proto.Envelope) string {
		calls <- env
		return `{"result":{"state":"syncing"}}`
	})
	if _, err := h.store.UpsertImageNode(&store.ImageNode{ImageID: h.image.ID, NodeID: h.node.ID, State: store.ImageStatePresent}); err != nil {
		t.Fatal(err)
	}
	fetch := imagecatalog.FetchRequest{Source: imagecatalog.FetchSourceLocal, ArchivePath: "/data/replacement.tar"}
	request := func() *store.ImageNode {
		t.Helper()
		row, err := h.hub.PullImage(t.Context(), h.image.ID, h.node.UUID, fetch)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if row := request(); row.State != store.ImageStatePresent || len(calls) != 0 {
		t.Fatalf("ordinary import = %+v, RPCs = %d", row, len(calls))
	}
	fetch.Force = true
	first := request()
	if first.State != store.ImageStateSyncing || first.TransferID == "" || len(calls) != 1 {
		t.Fatalf("replacement = %+v, RPCs = %d", first, len(calls))
	}
	if retry := request(); retry.TransferID != first.TransferID || len(calls) != 1 {
		t.Fatalf("replaced an active transfer: %+v", retry)
	}
}
