package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// maxModelSourceIDLen is the models.source_id column size.
const maxModelSourceIDLen = 255

// updateModel changes the source id of a local or URL model (PATCH
// /models/{id}). Such a model's source id is a label chosen at registration;
// correcting it to the repository id lets an inference template recognize the
// model. Files already copied keep their paths; later copies derive theirs
// from the new id.
func (a *apiHandlers) updateModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID *string `json:"source_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDInvalid, "invalid model update JSON", map[string]any{"detail": err.Error()})
		return
	}
	if req.SourceID == nil {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDInvalid, "source_id is required", map[string]any{"detail": "source_id is required"})
		return
	}
	sourceID := strings.Trim(strings.TrimSpace(*req.SourceID), "/")
	if detail := modelSourceIDProblem(sourceID); detail != "" {
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDInvalid, detail, map[string]any{"detail": detail})
		return
	}
	model, _, err := a.d.Store.SetModelSourceID(parseUint(chi.URLParam(r, "id")), sourceID)
	var taken *store.ModelSourceIDTakenError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, a.modelDTO(model, true))
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrModelSourceIDFixed):
		writeProductError(w, http.StatusBadRequest, ErrorModelSourceIDFixed, err.Error(), nil)
	case errors.As(err, &taken):
		writeProductError(w, http.StatusConflict, ErrorModelSourceIDTaken, err.Error(), map[string]any{"model_id": taken.ModelID})
	case errors.Is(err, store.ErrModelTransferActive):
		writeProductError(w, http.StatusConflict, ErrorModelTransferActive, err.Error(), nil)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}

// modelSourceIDProblem explains why a source id cannot be stored, or returns
// "" when it can.
func modelSourceIDProblem(sourceID string) string {
	switch {
	case sourceID == "":
		return "source_id must not be empty"
	case len(sourceID) > maxModelSourceIDLen:
		return "source_id must be at most 255 bytes"
	case !utf8.ValidString(sourceID):
		return "source_id must be valid UTF-8"
	case strings.IndexFunc(sourceID, unicode.IsControl) >= 0:
		return "source_id must not contain control characters"
	}
	return ""
}
