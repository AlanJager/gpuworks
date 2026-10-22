package api

import (
	"errors"
	"net/http"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// inferenceTemplateSchema serves the editor dictionary for the flat YAML:
// autocomplete, hover documentation, and first-pass client validation. The
// authoritative validation stays in ParseYAML + ValidateFlatCatalog.
func (a *apiHandlers) inferenceTemplateSchema(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, inferencecatalog.JSONSchema())
}

// inferenceTemplateYAML answers GET /inference-templates/yaml?model_id=…
// [&spec_source=user|upstream]: the flat YAML the template editor shows. The
// server writes it from the catalog types the import endpoint decodes
// (inferencecatalog.EditableYAML), so saving it back unchanged is lossless.
// model_id is a query parameter because its slashes do not fit a path segment.
func (a *apiHandlers) inferenceTemplateYAML(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimSpace(r.URL.Query().Get("model_id"))
	if modelID == "" {
		writeProductError(w, http.StatusBadRequest, messages.ErrorInferenceTemplateModelRequired, "model_id is required", nil)
		return
	}
	specSource := strings.TrimSpace(r.URL.Query().Get("spec_source"))
	if specSource != "" && specSource != "user" && specSource != "upstream" {
		writeProductError(w, http.StatusBadRequest, messages.ErrorInferenceTemplateSpecSourceInvalid, "spec_source must be user or upstream", map[string]any{"spec_source": specSource})
		return
	}
	model, err := a.d.Store.InferenceModelByModelID(modelID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeErr(w, status, err)
		return
	}
	data, err := inferencecatalog.EditableYAML(*model, specSource)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// deleteInferenceTemplateSpec retires one user-authored spec. Imports are
// additive, so this is the removal path. Query parameters keep model_id's
// slashes out of the path segment: DELETE /inference-templates/spec?model_id=…&spec=…
func (a *apiHandlers) deleteInferenceTemplateSpec(w http.ResponseWriter, r *http.Request) {
	modelID := strings.TrimSpace(r.URL.Query().Get("model_id"))
	specName := strings.TrimSpace(r.URL.Query().Get("spec"))
	if modelID == "" || specName == "" {
		writeProductError(w, http.StatusBadRequest, messages.ErrorInferenceSpecTargetRequired, "model_id and spec are required", nil)
		return
	}
	modelDeleted, err := a.d.Store.DeleteUserInferenceSpec(modelID, specName)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrInferenceSpecNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, store.ErrInferenceSpecInUse), errors.Is(err, store.ErrInferenceModelReferenced):
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	a.pruneUnreferencedCatalogPackages()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "model_deleted": modelDeleted})
}
