package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// PluginAdmin manages the installed plugin packages (/api/v1/plugins). The
// routes are core routes: never behind a feature, open to administrators
// with every plugin down or removed, and closed to the assistant and to
// platform tokens for writes. A refusal is a *messages.ProductError.
type PluginAdmin interface {
	List(ctx context.Context) []dto.Plugin
	// Check is the plugins diagnostics check.
	Check(ctx context.Context) dto.DiagnosticCheck
	// WriteMetrics writes the installed plugins' metrics in the
	// Prometheus text format.
	WriteMetrics(w io.Writer) error
	// Install verifies the package read from body and installs it when
	// approve is its approval hash; enable starts it once installed. by
	// names the administrator for the audit record.
	Install(ctx context.Context, body io.Reader, approve string, enable bool, by string) (dto.PluginInstalled, error)
	Enable(ctx context.Context, id, by string) (dto.Plugin, error)
	Disable(ctx context.Context, id, by string) (dto.Plugin, error)
	// Uninstall stops and removes the package (and its data with purge)
	// and releases what it pushed.
	Uninstall(ctx context.Context, id string, purge bool, by string) error
}

// pluginRoutes mounts /api/v1/plugins: administrators read the list by
// session or token; every change needs an administrator's login session.
func (a *apiHandlers) pluginRoutes(r chi.Router) {
	r.With(a.managementAuth, readTokensRead, a.managementAdmin, compressJSON).Get("/plugins", a.listPlugins)
	r.With(a.managementAuth, readTokensRead, a.managementAdmin).Get("/metrics/plugins", a.pluginMetrics)
	r.Group(func(r chi.Router) {
		r.Use(a.managementAuth, sessionOnly, a.managementAdmin)
		r.Post("/plugins/install", a.installPlugin)
		r.Post("/plugins/{id}/enable", a.enablePlugin)
		r.Post("/plugins/{id}/disable", a.disablePlugin)
		r.Delete("/plugins/{id}", a.uninstallPlugin)
	})
}

// pluginAdmin is the plugin administration this server runs, writing the
// refusal when it runs none or the assistant is asking.
func (a *apiHandlers) pluginAdmin(w http.ResponseWriter, r *http.Request) (PluginAdmin, bool) {
	if isAssistantRead(r) {
		writeProductError(w, http.StatusNotFound, messages.ErrorNotFound, "the assistant does not read this route", nil)
		return nil, false
	}
	p := a.d.Extensions.Plugins
	if p == nil {
		writeProductError(w, http.StatusServiceUnavailable, messages.ErrorPluginsUnavailable, "this server runs no plugin host", nil)
		return nil, false
	}
	return p, true
}

func (a *apiHandlers) listPlugins(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	items := p.List(r.Context())
	if items == nil {
		items = []dto.Plugin{}
	}
	out := dto.PluginList{Items: items}
	if len(items) > 0 {
		check := p.Check(r.Context())
		out.Check = &check
	}
	writeJSON(w, http.StatusOK, out)
}

// pluginMetrics serves the gpuworks_plugin_* metrics of the installed
// packages for a Prometheus scrape (text format 0.0.4), to administrators
// by session or token.
func (a *apiHandlers) pluginMetrics(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := p.WriteMetrics(&buf); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// installPlugin takes the package as the request body
// (application/octet-stream), ?approve=<approval hash> and ?enable=true.
func (a *apiHandlers) installPlugin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	enable := false
	if v := q.Get("enable"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "enable must be true or false", map[string]any{"field": "enable", "detail": "enable must be true or false"})
			return
		}
		enable = b
	}
	out, err := p.Install(r.Context(), r.Body, q.Get("approve"), enable, a.adminName(r))
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *apiHandlers) enablePlugin(w http.ResponseWriter, r *http.Request) {
	a.setPluginEnabled(w, r, true)
}

func (a *apiHandlers) disablePlugin(w http.ResponseWriter, r *http.Request) {
	a.setPluginEnabled(w, r, false)
}

func (a *apiHandlers) setPluginEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	id, by := chi.URLParam(r, "id"), a.adminName(r)
	var (
		out dto.Plugin
		err error
	)
	if enabled {
		out, err = p.Enable(r.Context(), id, by)
	} else {
		out, err = p.Disable(r.Context(), id, by)
	}
	if err != nil {
		writePluginError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// uninstallPlugin takes ?purge=true to remove the plugin's data too.
func (a *apiHandlers) uninstallPlugin(w http.ResponseWriter, r *http.Request) {
	p, ok := a.pluginAdmin(w, r)
	if !ok {
		return
	}
	purge := false
	if v := r.URL.Query().Get("purge"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "purge must be true or false", map[string]any{"field": "purge", "detail": "purge must be true or false"})
			return
		}
		purge = b
	}
	if err := p.Uninstall(r.Context(), chi.URLParam(r, "id"), purge, a.adminName(r)); err != nil {
		writePluginError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *apiHandlers) adminName(r *http.Request) string {
	claims, _ := a.claimsFromRequest(r)
	return claims.Username
}

// pluginErrorStatus maps a plugin administration refusal to its status:
// not installed 404; a choice that conflicts with what is installed
// (including a feature key another owner already provides) or asks for
// approval 409; a package too large 413; any other refused package 422;
// anything else 500.
func pluginErrorStatus(err error) int {
	var pe *messages.ProductError
	if !errors.As(err, &pe) {
		return http.StatusInternalServerError
	}
	switch pe.Code() {
	case messages.ErrorPluginNotInstalled:
		return http.StatusNotFound
	case messages.ErrorPluginApprovalRequired, messages.ErrorPluginBroken, messages.ErrorPluginVersionConflict, messages.ErrorPluginFeatureTaken, errorPluginStateTooNew:
		return http.StatusConflict
	case messages.ErrorPluginPackageTooLarge:
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusUnprocessableEntity
}

// errorPluginStateTooNew is pkg/extapi's code for a package whose data a
// newer build wrote (extapi.ErrorStateTooNew), which the plugin
// administration hands on as a product error.
const errorPluginStateTooNew = "PLUGIN_STATE_TOO_NEW"

func writePluginError(w http.ResponseWriter, err error) {
	writeErr(w, pluginErrorStatus(err), err)
}
