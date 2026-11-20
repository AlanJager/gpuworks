package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// The plugin administration's reports and the restart: administrators
// read the catalog, a package's pushed state and its uninstall impact by
// session or token; a restart needs an administrator's login session.

func (a *apiHandlers) restartPlugin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	out, err := p.Restart(r.Context(), chi.URLParam(r, "id"), a.adminName(r))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) pluginCatalog(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	out, err := p.Catalog(r.Context())
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) pluginState(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	out, err := p.State(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) pluginImpact(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	out, err := p.Impact(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
