package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

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
	ctx, cancel := rpccontext.WithTimeout(r.Context(), proto.ImageArchivePreviewTimeout)
	defer cancel()
	raw, err := a.d.CallNode(ctx, node.UUID, proto.RPCImagePreviewPath, req)
	if errors.Is(err, proto.ErrRPCUnknownMethod) {
		writeProductError(w, http.StatusConflict, messages.ErrorAgentUpgradeRequired, "upgrade the target agent to preview a node archive", nil)
		return
	}
	if err != nil {
		writeProductError(w, http.StatusConflict, messages.ErrorImageArchivePreviewFailed, "node archive preview failed", map[string]any{"detail": err.Error()})
		return
	}
	var result imagecatalog.ArchiveContents
	if err := json.Unmarshal(raw, &result); err != nil || result.RepoTags == nil {
		writeProductError(w, http.StatusBadGateway, messages.ErrorImageArchivePreviewFailed, "invalid node archive preview response", map[string]any{"detail": "invalid agent response"})
		return
	}
	imagecatalog.NormalizeArchiveEngines(result.MetadataByTag)
	writeJSON(w, http.StatusOK, result)
}
