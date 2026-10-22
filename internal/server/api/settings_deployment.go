package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// deploymentPolicyBody is PUT /settings/deployment: the cluster-wide pending
// timeout, instance start timeout and event retention. These bound every
// deployment, so only an administrator may change them, the way the terminal
// and monitor policies are guarded. The body is overlaid on the stored policy,
// so a form that edits one field leaves the others as they are, and the result
// is validated before it is written: a stored value that fails validation would
// only be discovered later, when DeploymentPolicy quietly falls back to the
// shipped defaults. It writes the error response itself; a non-nil error only
// tells the caller that a response has been written.
func (a *apiHandlers) deploymentPolicyBody(w http.ResponseWriter, r *http.Request, body []byte) ([]byte, error) {
	claims, ok := a.claimsFromRequest(r)
	if !ok || claims.Role != "admin" {
		writeProductError(w, http.StatusForbidden, ErrorAdministratorRequired, "administrator role required", nil)
		return nil, errDeploymentPolicyRefused
	}
	policy := a.d.Store.DeploymentPolicy()
	if err := json.Unmarshal(body, &policy); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, errDeploymentPolicyRefused
	}
	if err := store.ValidateDeploymentPolicy(policy); err != nil {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, err.Error(), map[string]any{"detail": err.Error()})
		return nil, errDeploymentPolicyRefused
	}
	out, err := json.Marshal(policy)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, errDeploymentPolicyRefused
	}
	return out, nil
}

var errDeploymentPolicyRefused = errors.New("deployment policy refused")

// deploymentPolicyJSON is the deployment policy GET /settings serves: the one
// every launch actually reads (Store.DeploymentPolicy), not the stored text.
// A policy stored before a key existed lacks it, and a form reading the raw
// document would show that field empty although the shipped default applies.
func (a *apiHandlers) deploymentPolicyJSON() json.RawMessage {
	raw, err := json.Marshal(a.d.Store.DeploymentPolicy())
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
