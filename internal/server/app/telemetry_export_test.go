package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func telemetryTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := auth.NewSecretbox("telemetry-app-test")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSecretbox(box)
	return st
}

// otlpSink is a minimal OTLP/HTTP receiver: it keeps each request's headers
// and decoded body (gzip honored) and answers 200 like the collector.
type otlpSink struct {
	server  *httptest.Server
	mu      sync.Mutex
	headers []http.Header
	bodies  []map[string]any
}

func newOTLPSink(t *testing.T) *otlpSink {
	s := &otlpSink{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			zr, err := gzip.NewReader(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			body = zr
		}
		var doc map[string]any
		if r.URL.Path != "/v1/traces" || json.NewDecoder(body).Decode(&doc) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, doc)
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *otlpSink) calls() ([]http.Header, []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...), append([]map[string]any(nil), s.bodies...)
}

func ptr[T any](v T) *T { return &v }

// The stored setting configures the exporter when the server starts; an
// update keeps a header's stored value when it is sent without one, is
// applied at once, and the answer never carries a header value.
func TestTelemetryServiceStoresAndAppliesTheSetting(t *testing.T) {
	st := telemetryTestStore(t)
	sink := newOTLPSink(t)
	if err := st.SetTelemetryExport(store.TelemetryExport{Enabled: true, Endpoint: sink.server.URL, SampleRatio: 1,
		Headers: []store.TelemetryHeader{{Name: "Authorization", Value: "Bearer first"}}}); err != nil {
		t.Fatal(err)
	}
	svc := newTelemetry(st, "127.0.0.1")
	t.Cleanup(svc.exporter.Close)
	got := svc.Export()
	if !got.Status.Active || got.Status.TracesURL != sink.server.URL+"/v1/traces" || got.Status.SettingError != "" {
		t.Fatalf("at start = %+v", got)
	}
	if !svc.exporter.Sample(tracecontext.NewTraceID(time.Now())) {
		t.Fatal("the stored ratio 1 does not sample")
	}

	out, err := svc.UpdateExport(dto.TelemetryExportUpdate{
		Enabled: true, Endpoint: " " + sink.server.URL + " ", SampleRatio: ptr(0.0), TraceLink: "https://grafana.example/explore?traceId={trace_id}",
		Headers: []dto.TelemetryHeaderUpdate{{Name: "authorization"}, {Name: "X-Scope-OrgID", Value: ptr("lab")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "Bearer first") || strings.Contains(string(raw), `"lab"`) {
		t.Fatalf("the answer carries a header value: %s", raw)
	}
	if out.Endpoint != sink.server.URL || out.SampleRatio != 0 || len(out.Headers) != 2 || out.Headers[0].Name != "authorization" || out.TraceLink == "" {
		t.Fatalf("update = %+v", out)
	}
	stored, err := st.TelemetryExport()
	if err != nil || len(stored.Headers) != 2 || stored.Headers[0].Value != "Bearer first" || stored.Headers[1].Value != "lab" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if svc.exporter.Sample(tracecontext.NewTraceID(time.Now())) {
		t.Fatal("ratio 0 still samples")
	}

	// A test uses the stored value of a header sent without one.
	res, err := svc.TestExport(context.Background(), dto.TelemetryExportUpdate{Endpoint: sink.server.URL, Headers: []dto.TelemetryHeaderUpdate{{Name: "Authorization"}}})
	if err != nil || !res.OK || res.TraceID == "" || res.Error != "" {
		t.Fatalf("test = %+v, %v", res, err)
	}
	headers, bodies := sink.calls()
	if len(bodies) != 1 || headers[0].Get("Authorization") != "Bearer first" {
		t.Fatalf("test reached the collector with %v", headers)
	}

	// Off: nothing is sampled; the endpoint may be anything.
	if _, err := svc.UpdateExport(dto.TelemetryExportUpdate{Endpoint: "not yet"}); err != nil {
		t.Fatal(err)
	}
	if got := svc.Export(); got.Status.Active || got.Enabled || got.SampleRatio != 1 || len(got.Headers) != 0 {
		t.Fatalf("off = %+v", got)
	}
}

func TestTelemetryServiceRefusesWhatItCannotUse(t *testing.T) {
	st := telemetryTestStore(t)
	svc := newTelemetry(st, "127.0.0.1")
	t.Cleanup(svc.exporter.Close)
	for code, u := range map[string]dto.TelemetryExportUpdate{
		api.ErrorTelemetryEndpointInvalid:     {Enabled: true, Endpoint: "collector:4318"},
		api.ErrorTelemetryHeaderValueRequired: {Headers: []dto.TelemetryHeaderUpdate{{Name: "Authorization"}}},
		api.ErrorTelemetryHeaderInvalid:       {Headers: []dto.TelemetryHeaderUpdate{{Name: "A", Value: ptr("1")}, {Name: "a", Value: ptr("2")}}},
		api.ErrorTelemetrySampleRatioInvalid:  {SampleRatio: ptr(1.5)},
		api.ErrorTelemetryTraceLinkInvalid:    {TraceLink: "https://grafana/explore"},
	} {
		_, err := svc.UpdateExport(u)
		var te *api.TelemetryError
		if !errors.As(err, &te) || te.Code != code {
			t.Errorf("%s: err = %v", code, err)
		}
	}
	for _, link := range []string{"ftp://x/{trace_id}", "{trace_id}", "https:///{trace_id}"} {
		if err := validTraceLink(link); err == nil {
			t.Errorf("trace link %q accepted", link)
		}
	}
	if raw := st.GetSetting(store.SettingTelemetryExport); raw != "" {
		t.Fatalf("a refused update was stored: %s", raw)
	}
	// A test always checks the endpoint, on or off.
	var te *api.TelemetryError
	if _, err := svc.TestExport(context.Background(), dto.TelemetryExportUpdate{Endpoint: "nope"}); !errors.As(err, &te) || te.Code != api.ErrorTelemetryEndpointInvalid {
		t.Fatalf("test of a bad endpoint: %v", err)
	}
}

// A stored setting that cannot be read leaves export off and says why.
func TestTelemetryServiceReportsAnUnusableStoredSetting(t *testing.T) {
	st := telemetryTestStore(t)
	if err := st.SetSetting(store.SettingTelemetryExport, `{"enabled":true,"endpoint":"http://c","headers":[{"name":"A","value_cipher":"garbage"}],"sample_ratio":1}`); err != nil {
		t.Fatal(err)
	}
	svc := newTelemetry(st, "127.0.0.1")
	t.Cleanup(svc.exporter.Close)
	got := svc.Export()
	if got.Status.Active || !strings.Contains(got.Status.SettingError, `"A"`) {
		t.Fatalf("export = %+v", got)
	}
	// Saving a good setting clears it (the header needs its value again).
	if _, err := svc.UpdateExport(dto.TelemetryExportUpdate{Headers: []dto.TelemetryHeaderUpdate{{Name: "A"}}}); err == nil {
		t.Fatal("a header whose stored value is unreadable was kept")
	}
	if got, err := svc.UpdateExport(dto.TelemetryExportUpdate{Endpoint: "http://c"}); err != nil || got.Status.SettingError != "" {
		t.Fatalf("after a good save: %+v, %v", got.Status, err)
	}
}

// Each finished request goes to the request store and to the exporter.
func TestTheRequestSinkFeedsBothTheStoreAndTheExporter(t *testing.T) {
	st := telemetryTestStore(t)
	sink := newOTLPSink(t)
	if err := st.SetTelemetryExport(store.TelemetryExport{Enabled: true, Endpoint: sink.server.URL, SampleRatio: 1}); err != nil {
		t.Fatal(err)
	}
	svc := newTelemetry(st, "127.0.0.1")
	var stored []uxrecord.Request
	feed := requestSink(func(r uxrecord.Request) { stored = append(stored, r) }, svc.exporter)
	trace := tracecontext.Begin(tracecontext.Parent{}, false, time.Now(), svc.exporter.Sample)
	feed(uxrecord.Request{ID: "r1", Trace: trace, ReceivedMS: time.Now().UnixMilli(), Path: "/v1/chat/completions", Status: 200, EndMS: 5,
		DispatchMS: 1, FirstByteMS: -1, FirstThinkMS: -1, FirstAnswerMS: -1, LastChunkMS: -1, MaxGapMS: -1,
		CachedTokens: -1, ReasoningTokens: -1, EngineQueueMS: -1, EnginePrefillMS: -1, EngineDecodeMS: -1})
	svc.exporter.Close()
	_, bodies := sink.calls()
	if len(stored) != 1 || len(bodies) != 1 || !strings.Contains(mustJSON(t, bodies[0]), trace.Trace.String()) {
		t.Fatalf("stored %d, exported %v", len(stored), bodies)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
