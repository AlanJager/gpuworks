package api

import (
	"encoding/json"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// getSecuritySettings returns the platform security switches; any signed-in
// user may read them (the playground needs the preview policy).
func (a *apiHandlers) getSecuritySettings(w http.ResponseWriter, r *http.Request) {
	policy := a.d.Store.SecurityPolicy()
	writeJSON(w, http.StatusOK, dto.SecuritySettings{PlaygroundPreviewNetwork: policy.PlaygroundPreviewNetwork})
}

// patchSecuritySettings changes the fields the body names and leaves the rest;
// the router admits administrators only.
func (a *apiHandlers) patchSecuritySettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PlaygroundPreviewNetwork *bool `json:"playground_preview_network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	policy := a.d.Store.SecurityPolicy()
	if req.PlaygroundPreviewNetwork != nil {
		policy.PlaygroundPreviewNetwork = *req.PlaygroundPreviewNetwork
	}
	if err := a.d.Store.SetSecurityPolicy(policy); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	a.getSecuritySettings(w, r)
}
