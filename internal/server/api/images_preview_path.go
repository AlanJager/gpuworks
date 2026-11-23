package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/imagecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/rpccontext"
)

// previewImageArchivePath reads tags on the selected node before a catalog
// version exists. The same path may name different archives on other nodes.
func (a *apiHandlers) previewImageArchivePath(w http.ResponseWriter, r *http.Request) {
	var req proto.ImagePreviewPathParams
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	req.ArchivePath = strings.TrimSpace(req.ArchivePath)
	if !filepath.IsAbs(req.ArchivePath) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorImageArchivePathInvalid, "archive_path must be absolute", nil)
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if a.d.CallNode == nil {
		writeProductError(w, http.StatusServiceUnavailable, ErrorImageInspectionUnavailable, "node image inspection is unavailable", nil)
		return
	}
	if a.d.Online != nil && !a.d.Online(node.UUID) {
		writeProductError(w, http.StatusConflict, ErrorNodeOffline, "node is offline", nil)
		return
	}
	a.auditNodeArchive(r, "preview", node.UUID, req.ArchivePath)
	// Give the worker time to report its own scan deadline over the control lane.
	ctx, cancel := rpccontext.WithTimeout(r.Context(), proto.ImageArchivePreviewTimeout+5*time.Second)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCImagePreviewPath, req)
	if errors.Is(err, proto.ErrRPCUnknownMethod) {
		writeProductError(w, http.StatusConflict, messages.ErrorAgentUpgradeRequired, "upgrade the target agent to preview a node archive", nil)
		return
	}
	if err != nil {
		writeProductError(w, http.StatusConflict, messages.ErrorImageArchivePreviewFailed, "node archive preview failed", map[string]any{"reason": archivePreviewReason(err)})
		return
	}
	var result imagecatalog.ArchiveContents
	if err := json.Unmarshal(raw, &result); err != nil || result.RepoTags == nil {
		writeProductError(w, http.StatusBadGateway, messages.ErrorImageArchivePreviewFailed, "invalid node archive preview response", nil)
		return
	}
	imagecatalog.NormalizeArchiveEngines(result.MetadataByTag)
	writeJSON(w, http.StatusOK, result)
}

func (a *apiHandlers) auditNodeArchive(r *http.Request, action, node, path string, details ...any) {
	if a.d.Log != nil {
		fields := []any{"action", action, "username", a.adminName(r), "node_uuid", node, "archive_path", path, "request_id", r.Header.Get("X-Request-ID")}
		a.d.Log.InfoContext(r.Context(), "node image archive request", append(fields, details...)...)
	}
}

func archivePreviewReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return proto.ArchivePreviewTimeout
	}
	var refusal *proto.RPCError
	if errors.As(err, &refusal) && refusal.Code == proto.ErrorArchivePreview {
		if reason, ok := refusal.Params["reason"].(string); ok {
			return reason
		}
	}
	return proto.ArchivePreviewUnreadable
}
