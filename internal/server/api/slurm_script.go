package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/slurmboot"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// slurmScriptRedacted stands for the enrollment token in the script an
// administrator reads (GET /slurm/clusters/{uuid}/script).
const slurmScriptRedacted = "REDACTED-ENROLLMENT-TOKEN"

// slurmClusterScript answers the setup script the platform serves the entry's
// controller, as text for an administrator to read before running it, the
// way the Kubernetes manifest is shown. Everything is as served except the
// enrollment token, which the script holds as its TOKEN parameter and which
// is not shown here: the commands that carry it are the ones to copy, and a
// script text that is read, scrolled and pasted around is no place for it.
func (a *apiHandlers) slurmClusterScript(w http.ResponseWriter, r *http.Request) {
	c, err := a.d.Store.SlurmClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	redacted := *c
	redacted.Token = slurmScriptRedacted
	params := slurmParams(redacted)
	if err := params.Validate(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(slurmboot.SetupScript(params)))
}
