package api

import (
	"errors"
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// slurmTokenStatus answers GET /slurm/token-status?token=: whether an
// enrollment token is active, revoked or unknown, and nothing else. The
// health check on every node asks before it installs an agent, so a revoked
// entry puts no new agent anywhere (agents already enrolled keep running, as
// with a revoked Kubernetes entry). It is open like the scripts: the token is
// the question's own secret, and the answer tells no more than enrolling with
// it would.
func (a *apiHandlers) slurmTokenStatus(w http.ResponseWriter, r *http.Request) {
	status := "unknown"
	c, err := a.d.Store.SlurmClusterByAnyToken(r.URL.Query().Get("token"))
	switch {
	case err == nil && c.RevokedAt != nil:
		status = "revoked"
		a.slurmActivity.nodeAsked(c.ID)
	case err == nil:
		status = "active"
		a.slurmActivity.nodeAsked(c.ID)
	case !errors.Is(err, store.ErrSlurmTokenUnknown):
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, dto.SlurmTokenStatus{Status: status})
}
