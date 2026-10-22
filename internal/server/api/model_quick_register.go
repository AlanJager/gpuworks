package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// QuickRegisterRequest is the small, non-blocking registration contract used
// when a deployment starts from an inference-template weight source. Metadata
// is deliberately best-effort and refreshed asynchronously after the model
// identity has been persisted.
type QuickRegisterRequest struct {
	Source            string `json:"source"`
	SourceID          string `json:"source_id"`
	Revision          string `json:"revision"`
	Name              string `json:"name"`
	WeightBytes       *int64 `json:"weight_bytes"`
	Quantization      string `json:"quantization"`
	Architecture      string `json:"architecture"`
	ContextLength     int    `json:"context_length"`
	ArtifactSubfolder string `json:"artifact_subfolder"`
}

// quickRegisterModel persists a template weight source without waiting for a
// repository metadata request. This keeps the deploy form responsive even
// when the model registry is slow or temporarily unavailable.
func (a *apiHandlers) quickRegisterModel(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	var req QuickRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !validateName(w, req.Name) {
		return
	}
	req.Source = strings.ToLower(strings.TrimSpace(req.Source))
	req.SourceID = strings.TrimSpace(req.SourceID)
	req.Revision = strings.TrimSpace(req.Revision)
	req.Name = strings.TrimSpace(req.Name)
	req.ArtifactSubfolder = strings.TrimSpace(req.ArtifactSubfolder)
	if req.Source == "" || req.SourceID == "" {
		writeProductError(w, http.StatusBadRequest, ErrorQuickRegisterFieldsRequired, "source and source_id are required", nil)
		return
	}
	if req.Source != "huggingface" && req.Source != "modelscope" {
		writeProductError(w, http.StatusBadRequest, ErrorQuickRegisterSourceUnsupported, "unsupported model source", map[string]any{"source": req.Source})
		return
	}
	if req.Source == "modelscope" && strings.EqualFold(req.Revision, "main") {
		req.Revision = "master"
	}
	if req.Name == "" {
		req.Name = req.SourceID
	}
	if !validateName(w, req.Name) {
		return
	}
	if existing, err := a.d.Store.FindModelArtifact(req.SourceID, req.Source, req.Revision, req.ArtifactSubfolder); err == nil {
		writeJSON(w, http.StatusOK, a.modelDTO(existing, true))
		return
	} else if err != store.ErrNotFound {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	weightBytes := int64(0)
	if req.WeightBytes != nil && *req.WeightBytes > 0 {
		weightBytes = *req.WeightBytes
	}
	entity := &store.Model{
		Source: req.Source, SourceID: req.SourceID, Revision: req.Revision,
		ArtifactSubfolder: req.ArtifactSubfolder, Name: req.Name,
		WeightBytes: weightBytes, QuantLabel: strings.TrimSpace(req.Quantization),
		Arch: strings.TrimSpace(req.Architecture), ContextLength: int64(req.ContextLength),
	}
	created, err := a.d.Store.FindOrCreateModel(entity)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// The request context is cancelled as soon as the client receives the
	// response. Use a detached context so metadata refresh can finish safely.
	go a.refreshQuickRegisteredMetadata(created.ID)
	writeJSON(w, http.StatusCreated, a.modelDTO(created, true))
}

func (a *apiHandlers) refreshQuickRegisteredMetadata(modelID uint) {
	ctx := context.Background()
	entity, err := a.d.Store.GetModel(modelID)
	if err != nil {
		a.quickRegisterLog("get model", modelID, err)
		return
	}
	meta, err := a.refreshModelMetadataFor(ctx, entity)
	if err != nil || !hasFetchedMetadata(meta) {
		if err == nil {
			err = fmt.Errorf("metadata source returned no usable metadata")
		}
		a.quickRegisterLog("refresh metadata", modelID, err)
		return
	}
	a.applyFetchedMetadata(ctx, entity, meta)
	if _, err := a.d.Store.UpsertModel(entity); err != nil {
		a.quickRegisterLog("save metadata", modelID, err)
	}
}

func (a *apiHandlers) quickRegisterLog(operation string, modelID uint, err error) {
	if a.d.Log != nil {
		a.d.Log.Warn("quick model registration metadata refresh failed", slog.String("operation", operation), slog.Uint64("model_id", uint64(modelID)), slog.Any("error", err))
	}
}
