package hub

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func (h *Hub) loadImageArchivePath(ctx context.Context, image *store.Image, target *store.Node, fetch imagecatalog.FetchRequest) (*store.ImageNode, error) {
	path := strings.TrimSpace(fetch.ArchivePath)
	if !filepath.IsAbs(path) {
		return nil, errors.New("archive_path must be an absolute file path on the target node")
	}
	transferID := auth.NewUUID()
	row, started, err := h.beginImageFetch(ctx, &store.ImageNode{
		ImageVersionID: image.ID, ImageID: image.ID, NodeID: target.ID,
		State: store.ImageStateSyncing, Source: imagecatalog.FetchSourceLocal,
		TransferID: transferID, Message: "loading image archive from node file",
	}, fetch.Force)
	if err != nil || !started {
		return row, err
	}
	if _, err := h.CallNode(ctx, target.UUID, proto.RPCImageLoadPath, proto.ImageLoadPathParams{
		CanonicalRef: image.ImageRef, ArchivePath: path, TransferID: transferID,
	}); err != nil {
		reason := proto.ArchiveLoadFailed
		var refusal *proto.RPCError
		if errors.As(err, &refusal) && refusal.Code == proto.ErrorArchiveLoad {
			value, _ := refusal.Params["reason"].(string)
			reason = proto.SafeArchiveLoadReason(value)
		} else if errors.Is(err, context.DeadlineExceeded) {
			reason = proto.ArchivePreviewTimeout
		}
		failure := messages.NewProductError(messages.ErrorImageArchiveLoadFailed, "node image archive could not be loaded", map[string]any{"reason": reason})
		if errors.Is(err, proto.ErrRPCUnknownMethod) {
			reason = proto.ArchiveLoadUpgrade
			failure = messages.NewProductError(messages.ErrorAgentUpgradeRequired, "the target node is running an older agent; upgrade the agent first", nil)
		}
		_ = h.store.FailImageTransferIfActive(image.ID, target.ID, transferID, reason)
		return nil, failure
	}
	h.watchDirectImageTransfer(image.ID, target.ID)
	return row, nil
}
