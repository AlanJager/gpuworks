package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// updateSlurmCluster edits an entry's settings. Agents take the settings
// they apply to their placeholder jobs from the server on every report
// (slurmReportInterval), so an edit reaches the nodes by itself and nothing
// is re-run on the cluster; the answer names what changed.
func (a *apiHandlers) updateSlurmCluster(w http.ResponseWriter, r *http.Request) {
	// Unknown fields are refused: a server URL, account or directory sent here
	// would otherwise look accepted.
	var req dto.SlurmClusterUpdate
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	old, err := a.d.Store.SlurmClusterByUUID(chi.URLParam(r, "uuid"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	c := *old
	text := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	text(&c.Name, req.Name)
	text(&c.AccountUser, req.AccountUser)
	text(&c.SbatchArgs, req.SbatchArgs)
	if req.IdleReturnSeconds != nil {
		c.IdleReturnSeconds = *req.IdleReturnSeconds
	}
	if req.PendingTimeoutSeconds != nil {
		c.PendingTimeoutSeconds = *req.PendingTimeoutSeconds
	}
	if !slurmValid(w, c) {
		return
	}
	updated, err := a.d.Store.UpdateSlurmCluster(c)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
		return
	case errors.Is(err, store.ErrSlurmClusterNameTaken):
		writeProductError(w, http.StatusConflict, ErrorSlurmClusterNameTaken, "another Slurm cluster is named "+c.Name, map[string]any{"name": c.Name})
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	changed := slurmChangedFields(*old, *updated)
	if a.d.Log != nil && len(changed) > 0 {
		a.d.Log.Info("slurm cluster edited", "cluster", updated.Name, "changed", strings.Join(changed, ","))
	}
	view, err := a.slurmClusterView(r.Context(), *updated, true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, dto.SlurmClusterEdit{Cluster: view, Changed: changed})
}

// slurmChangedFields names, by their JSON names and sorted, the editable
// settings that differ between two versions of an entry.
func slurmChangedFields(a, b store.SlurmCluster) []string {
	out := []string{}
	add := func(name string, differ bool) {
		if differ {
			out = append(out, name)
		}
	}
	add("name", a.Name != b.Name)
	add("account_user", a.AccountUser != b.AccountUser)
	add("sbatch_args", a.SbatchArgs != b.SbatchArgs)
	add("idle_return_seconds", a.IdleReturnSeconds != b.IdleReturnSeconds)
	add("pending_timeout_seconds", a.PendingTimeoutSeconds != b.PendingTimeoutSeconds)
	sort.Strings(out)
	return out
}
