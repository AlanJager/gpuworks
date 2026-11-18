package api

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// PluginAdministrator is who changes a restriction override: the user id
// the override records and the username the audit line names.
type PluginAdministrator struct {
	UserID   uint
	Username string
}

// maxOverrideReason is the longest reason an override records, in bytes.
const maxOverrideReason = 1024

// pluginOverrideRoutes mounts /api/v1/plugins/overrides: the list on the
// chain of GET /plugins, every change by an administrator's login session.
func (a *apiHandlers) pluginOverrideRoutes(r chi.Router) {
	r.With(a.managementAuth, readTokensRead, a.managementAdmin, compressJSON).Get("/plugins/overrides", a.listPluginOverrides)
	r.Group(func(r chi.Router) {
		r.Use(a.managementAuth, sessionOnly, a.managementAdmin)
		r.Post("/plugins/overrides", a.createPluginOverride)
		r.Delete("/plugins/overrides/{oid}", a.removePluginOverride)
	})
}

func (a *apiHandlers) listPluginOverrides(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	items := p.Overrides(r.Context())
	if items == nil {
		items = []dto.PluginOverride{}
	}
	writeJSON(w, http.StatusOK, dto.PluginOverrideList{Items: items})
}

// createPluginOverride takes a dto.PluginOverrideRequest. The shape is
// checked here (400); whether the owner holds the entry and the action fits
// it is pushed state's rule (PLUGIN_OVERRIDE_REJECTED, 422).
func (a *apiHandlers) createPluginOverride(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	var req dto.PluginOverrideRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	for _, f := range []struct{ name, value string }{{"owner", req.Owner}, {"kind", req.Kind}, {"entry", req.Entry}, {"reason", req.Reason}} {
		if strings.TrimSpace(f.value) == "" {
			writeOverrideFieldInvalid(w, f.name, f.name+" is required")
			return
		}
	}
	if req.Action != "delete" && req.Action != "loosen" {
		writeOverrideFieldInvalid(w, "action", "action must be delete or loosen")
		return
	}
	if len(strings.TrimSpace(req.Reason)) > maxOverrideReason || !utf8.ValidString(req.Reason) {
		writeOverrideFieldInvalid(w, "reason", "reason must be valid text of at most 1024 bytes")
		return
	}
	out, err := p.Override(r.Context(), req, a.administrator(r))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *apiHandlers) removePluginOverride(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "oid"), 10, 64)
	if err != nil || id <= 0 {
		writeOverrideFieldInvalid(w, "oid", "the override id is a positive integer")
		return
	}
	if err := p.RemoveOverride(r.Context(), id, a.administrator(r)); err != nil {
		writePluginError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeOverrideFieldInvalid(w http.ResponseWriter, field, detail string) {
	writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, detail, map[string]any{"field": field, "detail": detail})
}

// administrator is the signed-in administrator, for the override record
// and its audit line.
func (a *apiHandlers) administrator(r *http.Request) PluginAdministrator {
	return PluginAdministrator{UserID: a.adminID(r), Username: a.adminName(r)}
}

func (a *apiHandlers) adminID(r *http.Request) uint {
	claims, _ := a.claimsFromRequest(r)
	return claims.UserID
}
