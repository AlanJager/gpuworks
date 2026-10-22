package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// retryModelCache asks the controller to restart a failed cache copy
// (controller.RetryCacheCopy); the controller owns cache intents.
func (a *apiHandlers) retryModelCache(w http.ResponseWriter, r *http.Request) {
	var request struct {
		StorageID uint `json:"storage_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.StorageID == 0 {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "storage_id is required", map[string]any{"field": "storage_id", "detail": "storage_id is required"})
		return
	}
	node, err := a.d.Store.NodeByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		writeStoreErr(w, err)
		return
	}
	ref := store.FileRef{ModelID: parseUint(chi.URLParam(r, "id")), NodeID: node.ID, StorageID: request.StorageID}
	if _, err := a.d.Store.ModelFileAt(ref); err != nil {
		writeStoreErr(w, err)
		return
	}
	if a.d.Preparation == nil {
		writeProductError(w, http.StatusConflict, ErrorModelCacheRetryBlocked, "cache copy retry is not available", nil)
		return
	}
	var blocked *planning.CacheRetryBlocked
	switch err := a.d.Preparation.RetryCacheCopy(r.Context(), ref); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
	case errors.As(err, &blocked):
		writeProductError(w, http.StatusConflict, ErrorModelCacheRetryBlocked, blocked.Detail, nil)
	case errors.Is(err, store.ErrStateChanged):
		writeProductError(w, http.StatusConflict, ErrorModelCacheRetryBlocked, "the copy changed while retrying; refresh and try again", nil)
	default:
		writeStoreErr(w, err)
	}
}
