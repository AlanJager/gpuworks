package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/provider"
)

// testProviderConnection tests explicit (unsaved) credentials, streaming each
// check over SSE. The test outcome is payload, not the HTTP status: a failed
// connection still answers 200 with failed checks inside.
func (a *apiHandlers) testProviderConnection(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	var req struct {
		Kind       string            `json:"kind"`
		Endpoint   string            `json:"endpoint"`
		APIKey     string            `json:"api_key"`
		PublicKey  string            `json:"public_key"`
		PrivateKey string            `json:"private_key"`
		Extra      map[string]string `json:"extra"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := a.d.Providers.KindCapabilities(req.Kind); !ok {
		writeProductError(w, http.StatusBadRequest, messages.ErrorProviderKindUnknown, "unknown provider kind", map[string]any{"kind": req.Kind})
		return
	}
	a.streamConnectionTest(w, r, func(ctx context.Context, report provider.ReportFunc) (provider.Summary, error) {
		return a.d.Providers.TestConnection(ctx, req.Kind, provider.Credentials{
			Endpoint: req.Endpoint, APIKey: req.APIKey,
			PublicKey: req.PublicKey, PrivateKey: req.PrivateKey, Extra: req.Extra,
		}, report)
	})
}

// testSavedProviderConnection tests a stored provider without exposing its
// credentials. The body is optional: its non-empty fields replace the stored
// ones for this test only (an edit form's unsaved changes), and a blank
// secret means the stored value, as in an update.
func (a *apiHandlers) testSavedProviderConnection(w http.ResponseWriter, r *http.Request) {
	if a.d.Providers == nil {
		writeErr(w, http.StatusServiceUnavailable, errNoProviders)
		return
	}
	id := parseUint(chi.URLParam(r, "id"))
	if _, err := a.d.Store.GetProvider(id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req struct {
		Endpoint   string            `json:"endpoint"`
		APIKey     string            `json:"api_key"`
		PublicKey  string            `json:"public_key"`
		PrivateKey string            `json:"private_key"`
		Extra      map[string]string `json:"extra"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	overrides := provider.Credentials{
		Endpoint: req.Endpoint, APIKey: req.APIKey,
		PublicKey: req.PublicKey, PrivateKey: req.PrivateKey, Extra: req.Extra,
	}
	a.streamConnectionTest(w, r, func(ctx context.Context, report provider.ReportFunc) (provider.Summary, error) {
		return a.d.Providers.TestSavedConnection(ctx, id, overrides, report)
	})
}

// streamConnectionTest streams one run: a check frame per check, then one
// done frame. A run that cannot finish ends with a coded done frame; its
// cause may name stored configuration, so it goes to the server log only.
func (a *apiHandlers) streamConnectionTest(w http.ResponseWriter, r *http.Request, run func(context.Context, provider.ReportFunc) (provider.Summary, error)) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errStreamingUnsupported)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	firstFailedCode := ""
	report := func(c provider.Check) {
		if c.Status == provider.StatusFailed && firstFailedCode == "" {
			firstFailedCode = c.Code
		}
		writeSSE(w, "check", c)
		fl.Flush()
	}
	summary, err := run(r.Context(), report)
	if r.Context().Err() != nil {
		// The client closed the dialog: nobody reads a done frame, and the
		// interrupted run is not a finding about the provider.
		return
	}
	if err != nil {
		slog.Default().Warn("provider connection test did not finish", "path", r.URL.Path, "err", err)
		writeSSE(w, "done", struct {
			OK   bool   `json:"ok"`
			Code string `json:"code"`
		}{Code: messages.ErrorProviderTestUnavailable})
		fl.Flush()
		noteProductError(w, messages.ErrorProviderTestUnavailable, nil)
		return
	}
	writeSSE(w, "done", summary)
	fl.Flush()
	// The response streams 200 even when checks fail; note the first failure's
	// product code so the access log's request line carries the reason.
	if firstFailedCode != "" {
		noteProductError(w, firstFailedCode, nil)
	}
}
