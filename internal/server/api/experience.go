package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Experience is the experience environment beside this server
// (internal/server/experiencelaunch): a separate console with simulated
// nodes and data, for showing the product.
type Experience interface {
	Status(ctx context.Context) dto.ExperienceStatus
	// SetEnabled starts the environment from its initial data with a new
	// password, or stops it.
	SetEnabled(ctx context.Context, on bool) error
	// Reset starts a running environment again from its initial data.
	Reset(ctx context.Context) error
	// RotatePassword gives the running environment's console a new password.
	RotatePassword(ctx context.Context) error
}

func (a *apiHandlers) getExperience(w http.ResponseWriter, r *http.Request) {
	if a.d.Experience == nil {
		writeProductError(w, http.StatusNotFound, ErrorExperienceUnavailable, "this server has no experience environment", nil)
		return
	}
	writeJSON(w, http.StatusOK, a.d.Experience.Status(r.Context()))
}

func (a *apiHandlers) putExperienceEnabled(w http.ResponseWriter, r *http.Request) {
	var req dto.ExperienceEnabled
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeProductError(w, http.StatusBadRequest, ErrorExperienceInvalid, "enabled must be true or false", nil)
		return
	}
	a.experienceAction(w, r, func(e Experience) error { return e.SetEnabled(r.Context(), *req.Enabled) })
}

func (a *apiHandlers) postExperienceReset(w http.ResponseWriter, r *http.Request) {
	a.experienceAction(w, r, func(e Experience) error { return e.Reset(r.Context()) })
}

func (a *apiHandlers) postExperiencePassword(w http.ResponseWriter, r *http.Request) {
	a.experienceAction(w, r, func(e Experience) error { return e.RotatePassword(r.Context()) })
}

func (a *apiHandlers) experienceAction(w http.ResponseWriter, r *http.Request, act func(Experience) error) {
	if a.d.Experience == nil {
		writeProductError(w, http.StatusNotFound, ErrorExperienceUnavailable, "this server has no experience environment", nil)
		return
	}
	if err := act(a.d.Experience); err != nil {
		var product *messages.ProductError
		if errors.As(err, &product) {
			writeErr(w, http.StatusConflict, err)
			return
		}
		writeProductError(w, http.StatusConflict, ErrorExperienceFailed, "experience environment: "+err.Error(), map[string]any{"reason": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, a.d.Experience.Status(r.Context()))
}
