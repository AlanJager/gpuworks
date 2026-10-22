package app

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/traceexport"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
	"gpuworks.ai/gpuworks/internal/version"
)

// telemetryService is api.TelemetryService: the telemetry export setting is
// the one source of the exporter's configuration (ADR 0046). It is applied
// when the server starts and whenever the API saves it; nothing else writes
// it (the generic settings route refuses the key).
type telemetryService struct {
	store    *store.Store
	exporter *traceexport.Exporter

	mu sync.Mutex // serializes updates
	// settingErr is why the stored setting could not be applied; "" when
	// it was.
	settingErr string
}

// newTelemetry starts the exporter for this control plane and applies the
// stored setting. A setting that does not decrypt or validate leaves export
// off and is reported in the status rather than failing the server.
func newTelemetry(st *store.Store, allowedPrivateHosts ...string) *telemetryService {
	identity := st.ControlPlaneIdentity()
	exporter := traceexport.New(traceexport.Options{AllowPrivateHosts: allowedPrivateHosts, Resource: traceexport.Resource{
		ServiceVersion: version.Version, InstanceID: identity.ID, Name: identity.Name,
	}})
	t := &telemetryService{store: st, exporter: exporter}
	stored, err := st.TelemetryExport()
	if err == nil {
		err = exporter.Configure(exportConfig(stored))
	}
	if err != nil {
		t.settingErr = err.Error()
	}
	return t
}

// requestSink hands each finished request's record to the request store and
// to the trace exporter; neither blocks.
func requestSink(ux func(uxrecord.Request), traces *traceexport.Exporter) func(uxrecord.Request) {
	return func(r uxrecord.Request) {
		ux(r)
		traces.Observe(r)
	}
}

func exportConfig(t store.TelemetryExport) traceexport.Config {
	cfg := traceexport.Config{Enabled: t.Enabled, Endpoint: t.Endpoint, SampleRatio: t.SampleRatio}
	if len(t.Headers) > 0 {
		cfg.Headers = make(map[string]string, len(t.Headers))
		for _, h := range t.Headers {
			cfg.Headers[h.Name] = h.Value
		}
	}
	return cfg
}

func (t *telemetryService) Export() dto.TelemetryExport {
	stored, err := t.store.TelemetryExport()
	out := dto.TelemetryExport{Endpoint: stored.Endpoint, Enabled: stored.Enabled, SampleRatio: stored.SampleRatio,
		TraceLink: stored.TraceLink, Headers: []dto.TelemetryHeaderName{}}
	if err == nil && !stored.Enabled && stored.Endpoint == "" && stored.SampleRatio == 0 {
		out.SampleRatio = 1 // never set: the default an operator turning it on starts from
	}
	for _, h := range stored.Headers {
		out.Headers = append(out.Headers, dto.TelemetryHeaderName{Name: h.Name})
	}
	st := t.exporter.Status()
	out.Status = dto.TelemetryExportStatus{
		Active: st.Enabled, TracesURL: st.Endpoint, Exported: st.Exported, Dropped: st.Dropped, Failed: st.Failed,
		Queued: st.Queued, LastError: st.LastError, LastErrorAt: unixMS(st.LastErrorAt), LastExportAt: unixMS(st.LastExportAt),
	}
	t.mu.Lock()
	out.Status.SettingError = t.settingErr
	t.mu.Unlock()
	if err != nil {
		out.Status.SettingError = err.Error()
	}
	return out
}

func unixMS(at time.Time) *int64 {
	if at.IsZero() {
		return nil
	}
	ms := at.UnixMilli()
	return &ms
}

func (t *telemetryService) UpdateExport(update dto.TelemetryExportUpdate) (dto.TelemetryExport, error) {
	t.mu.Lock()
	next, cfg, err := t.resolve(update, update.Enabled)
	if err == nil {
		err = t.store.SetTelemetryExport(next)
	}
	if err == nil {
		// Validated above, so Configure takes it.
		err = t.exporter.Configure(cfg)
		t.settingErr = ""
	}
	t.mu.Unlock()
	if err != nil {
		return dto.TelemetryExport{}, err
	}
	return t.Export(), nil
}

func (t *telemetryService) TestExport(ctx context.Context, update dto.TelemetryExportUpdate) (dto.TelemetryExportTest, error) {
	t.mu.Lock()
	_, cfg, err := t.resolve(update, true)
	t.mu.Unlock()
	if err != nil {
		return dto.TelemetryExportTest{}, err
	}
	res, err := t.exporter.Test(ctx, cfg)
	out := dto.TelemetryExportTest{OK: err == nil, TraceID: res.TraceID, TracesURL: res.URL}
	if err != nil {
		out.Error = err.Error()
	}
	return out, nil
}

// resolve turns an update into the setting to store and the exporter
// configuration it means, or a *api.TelemetryError. A header without a value
// takes the stored one of that name; checkEndpoint validates the endpoint
// even when export stays off (a test always sends). The caller holds t.mu.
func (t *telemetryService) resolve(update dto.TelemetryExportUpdate, checkEndpoint bool) (store.TelemetryExport, traceexport.Config, error) {
	next := store.TelemetryExport{Enabled: update.Enabled, Endpoint: strings.TrimSpace(update.Endpoint), SampleRatio: 1, TraceLink: strings.TrimSpace(update.TraceLink)}
	if update.SampleRatio != nil {
		next.SampleRatio = *update.SampleRatio
	}
	var stored []store.TelemetryHeader
	if current, err := t.store.TelemetryExport(); err == nil {
		stored = current.Headers
	}
	seen := map[string]bool{}
	for _, h := range update.Headers {
		name := strings.TrimSpace(h.Name)
		if seen[strings.ToLower(name)] {
			return next, traceexport.Config{}, &api.TelemetryError{Code: api.ErrorTelemetryHeaderInvalid, Fallback: "header " + name + " is given twice", Params: map[string]any{"name": name}}
		}
		seen[strings.ToLower(name)] = true
		value, found := "", false
		if h.Value != nil {
			value, found = *h.Value, true
		} else {
			for _, s := range stored {
				if strings.EqualFold(s.Name, name) {
					value, found = s.Value, true
				}
			}
		}
		if !found {
			return next, traceexport.Config{}, &api.TelemetryError{Code: api.ErrorTelemetryHeaderValueRequired, Fallback: "header " + name + " needs a value", Params: map[string]any{"name": name}}
		}
		next.Headers = append(next.Headers, store.TelemetryHeader{Name: name, Value: value})
	}
	cfg := exportConfig(next)
	check := cfg
	check.Enabled = check.Enabled || checkEndpoint
	if err := check.Validate(); err != nil {
		return next, cfg, telemetryConfigError(err)
	}
	if err := validTraceLink(next.TraceLink); err != nil {
		return next, cfg, err
	}
	return next, cfg, nil
}

func telemetryConfigError(err error) error {
	var ce *traceexport.ConfigError
	if !errors.As(err, &ce) {
		return err
	}
	switch ce.Field {
	case "endpoint":
		return &api.TelemetryError{Code: api.ErrorTelemetryEndpointInvalid, Fallback: ce.Msg}
	case "header":
		return &api.TelemetryError{Code: api.ErrorTelemetryHeaderInvalid, Fallback: ce.Msg, Params: map[string]any{"name": ce.Header}}
	default:
		return &api.TelemetryError{Code: api.ErrorTelemetrySampleRatioInvalid, Fallback: ce.Msg}
	}
}

// traceIDPlaceholder is where a trace link takes the trace id.
const traceIDPlaceholder = "{trace_id}"

// validTraceLink accepts empty, or an http(s) URL holding {trace_id}.
func validTraceLink(link string) error {
	if link == "" {
		return nil
	}
	u, err := url.Parse(strings.ReplaceAll(link, traceIDPlaceholder, "0"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || !strings.Contains(link, traceIDPlaceholder) {
		return &api.TelemetryError{Code: api.ErrorTelemetryTraceLinkInvalid, Fallback: "the trace link must be an http(s) URL containing {trace_id}"}
	}
	return nil
}
