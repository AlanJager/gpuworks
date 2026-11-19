package api

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

// upgradePlugin takes the new package as the request body
// (application/octet-stream) and ?approve=<approval hash>. It answers once
// the new version passed its health gate or was rolled back, which takes
// up to about 80 seconds; the upgrade runs to its end even when the client
// goes away.
func (a *apiHandlers) upgradePlugin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	out, err := p.Upgrade(r.Context(), chi.URLParam(r, "id"), r.Body, r.URL.Query().Get("approve"), a.adminName(r))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// rollbackPlugin takes ?restore_state=true to put the data back from the
// snapshot of the previous version.
func (a *apiHandlers) rollbackPlugin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	restore := false
	if v := r.URL.Query().Get("restore_state"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "restore_state must be true or false",
				map[string]any{"field": "restore_state", "detail": "restore_state must be true or false"})
			return
		}
		restore = b
	}
	out, err := p.Rollback(r.Context(), chi.URLParam(r, "id"), restore, a.adminName(r))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
