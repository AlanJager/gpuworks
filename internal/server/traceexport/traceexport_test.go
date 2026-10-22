package traceexport

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
	gatewaySpan = "b7ad6b7169203331"
)

func traceSpan(t *testing.T, sampled bool, parent bool) tracecontext.Span {
	t.Helper()
	flags := "00"
	if sampled {
		flags = "01"
	}
	p, ok := tracecontext.ParseTraceparent([]string{"00-" + callerTrace + "-" + callerSpan + "-" + flags}, []string{"vendor=a"})
	if !ok {
		t.Fatal("bad parent")
	}
	sp := tracecontext.Span{Trace: p.Trace, Flags: p.Flags, State: p.State}
	if parent {
		sp.Parent = p.Span
	} else {
		sp.State = ""
	}
	id, _ := hex.DecodeString(gatewaySpan)
	copy(sp.ID[:], id)
	return sp
}

// streamedChat is a finished, streamed chat request that reasoned, with
// the engine's usage and phases.
func streamedChat(t *testing.T) uxrecord.Request {
	return uxrecord.Request{
		ID: "gw/req-1", Trace: traceSpan(t, true, true), Model: "qwen3-8b",
		Sampling:   uxrecord.Sampling{MaxTokens: 512, HasMaxTokens: true, Temperature: 0.6, HasTemperature: true, TopP: 0.95, HasTopP: true, N: 1, HasN: true},
		ReceivedMS: 1760500000000, DeploymentID: 7, InstanceUUID: "inst-a", APIKeyID: 3, UserID: 1,
		Source: uxrecord.SourceAPI, Path: "/v1/chat/completions", Stream: true, BodyBytes: 2048,
		DispatchMS: 2, FirstByteMS: 40, FirstThinkMS: 120, FirstAnswerMS: 900, LastChunkMS: 3000, EndMS: 3005,
		Events: 200, ThinkEvents: 50, MaxGapMS: 80, Status: 200, Outcome: uxrecord.OutcomeOK, FinishReason: "stop",
		Usage: true, PromptTokens: 1500, CompletionTokens: 400, CachedTokens: 1024, ReasoningTokens: 120,
		EngineQueueMS: 5, EnginePrefillMS: 80, EngineDecodeMS: 2900, Weight: 1,
	}
}

// The span of a request, on the wire, field by field.
func TestASpanOnTheWire(t *testing.T) {
	raw, _, err := encodeRecords(Resource{ServiceVersion: "v1.2.3", InstanceID: "cp-1", Name: "lab"}, []uxrecord.Request{streamedChat(t)})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"resourceSpans":[{"resource":{"attributes":[` +
		`{"key":"service.name","value":{"stringValue":"gpuworks"}},{"key":"service.version","value":{"stringValue":"v1.2.3"}},` +
		`{"key":"service.instance.id","value":{"stringValue":"cp-1"}},{"key":"gpuworks.control_plane.name","value":{"stringValue":"lab"}}]},` +
		`"scopeSpans":[{"scope":{"name":"gpuworks.gateway","version":"v1.2.3"},"spans":[{` +
		`"traceId":"` + callerTrace + `","spanId":"` + gatewaySpan + `","traceState":"vendor=a","parentSpanId":"` + callerSpan + `",` +
		`"flags":769,"name":"chat qwen3-8b","kind":2,"startTimeUnixNano":"1760500000000000000","endTimeUnixNano":"1760500003005000000",` +
		`"attributes":[` +
		`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},` +
		`{"key":"gen_ai.request.model","value":{"stringValue":"qwen3-8b"}},` +
		`{"key":"gen_ai.request.max_tokens","value":{"intValue":"512"}},` +
		`{"key":"gen_ai.request.temperature","value":{"doubleValue":0.6}},` +
		`{"key":"gen_ai.request.top_p","value":{"doubleValue":0.95}},` +
		`{"key":"gen_ai.request.choice.count","value":{"intValue":"1"}},` +
		`{"key":"gen_ai.request.stream","value":{"boolValue":true}},` +
		`{"key":"gen_ai.usage.input_tokens","value":{"intValue":"1500"}},` +
		`{"key":"gen_ai.usage.output_tokens","value":{"intValue":"400"}},` +
		`{"key":"gen_ai.usage.cache_read.input_tokens","value":{"intValue":"1024"}},` +
		`{"key":"gen_ai.usage.reasoning.output_tokens","value":{"intValue":"120"}},` +
		`{"key":"gen_ai.response.finish_reasons","value":{"arrayValue":{"values":[{"stringValue":"stop"}]}}},` +
		`{"key":"gen_ai.response.time_to_first_chunk","value":{"doubleValue":0.12}},` +
		`{"key":"url.path","value":{"stringValue":"/v1/chat/completions"}},` +
		`{"key":"http.response.status_code","value":{"intValue":"200"}},` +
		`{"key":"gpuworks.request.id","value":{"stringValue":"gw/req-1"}},` +
		`{"key":"gpuworks.request.source","value":{"stringValue":"api"}},` +
		`{"key":"gpuworks.request.result","value":{"stringValue":"completed"}},` +
		`{"key":"gpuworks.request.body_bytes","value":{"intValue":"2048"}},` +
		`{"key":"gpuworks.deployment.id","value":{"intValue":"7"}},` +
		`{"key":"gpuworks.instance.uuid","value":{"stringValue":"inst-a"}},` +
		`{"key":"gpuworks.api_key.id","value":{"intValue":"3"}},` +
		`{"key":"gpuworks.user.id","value":{"intValue":"1"}},` +
		`{"key":"gpuworks.stream.events","value":{"intValue":"200"}},` +
		`{"key":"gpuworks.stream.max_gap_ms","value":{"intValue":"80"}},` +
		`{"key":"gen_ai.latency.e2e","value":{"doubleValue":3.005}},` +
		`{"key":"gen_ai.latency.time_in_queue","value":{"doubleValue":0.005}},` +
		`{"key":"gen_ai.latency.time_in_model_prefill","value":{"doubleValue":0.08}},` +
		`{"key":"gen_ai.latency.time_in_model_decode","value":{"doubleValue":2.9}},` +
		`{"key":"gen_ai.latency.time_in_model_inference","value":{"doubleValue":2.98}},` +
		`{"key":"gpuworks.phase.gateway_ms","value":{"doubleValue":2}},` +
		`{"key":"gpuworks.phase.preprocess_ms","value":{"doubleValue":33}},` +
		`{"key":"gpuworks.phase.thinking_ms","value":{"doubleValue":780}},` +
		`{"key":"gpuworks.phase.answering_ms","value":{"doubleValue":2100}},` +
		`{"key":"gpuworks.phase.tail_ms","value":{"doubleValue":5}}],` +
		`"events":[{"timeUnixNano":"1760500000002000000","name":"gpuworks.dispatch"},` +
		`{"timeUnixNano":"1760500000040000000","name":"gpuworks.first_byte"},` +
		`{"timeUnixNano":"1760500000120000000","name":"gpuworks.first_reasoning"},` +
		`{"timeUnixNano":"1760500000900000000","name":"gpuworks.first_answer"},` +
		`{"timeUnixNano":"1760500003000000000","name":"gpuworks.last_chunk"}],` +
		`"status":{}}]}]}]}`
	if string(raw) != want {
		t.Fatalf("span =\n%s\nwant\n%s", raw, want)
	}
}

// The gateway's phases and the engine's queue and prefill on the span are the
// request view's split: together they add up to the end-to-end time.
func TestTheSpanPhasesAddUpToTheRequest(t *testing.T) {
	r := streamedChat(t)
	sp := spanOf(r)
	sum := 0.0
	for _, a := range sp.Attributes {
		switch {
		case strings.HasPrefix(a.Key, "gpuworks.phase."):
			sum += a.Value.f
		case a.Key == "gen_ai.latency.time_in_queue", a.Key == "gen_ai.latency.time_in_model_prefill":
			sum += a.Value.f * 1000
		case a.Key == "gen_ai.latency.e2e" && a.Value.f*1000 != float64(r.EndMS):
			t.Fatalf("e2e = %v s", a.Value.f)
		}
	}
	if math.Abs(sum-float64(r.EndMS)) > 1e-6 {
		t.Fatalf("phases add up to %v, want %d", sum, r.EndMS)
	}
}

// What is not known is left out; failures on the platform's side are
// errors, a client leaving or a 4xx is not.
func TestSpanOfUnknownsAndFailures(t *testing.T) {
	base := uxrecord.Request{
		ID: "r", Trace: traceSpan(t, true, false), ReceivedMS: 1000, Path: "/v1/rerank", DeploymentID: 1,
		DispatchMS: 1, FirstByteMS: uxrecord.Unknown, FirstThinkMS: uxrecord.Unknown, FirstAnswerMS: uxrecord.Unknown,
		LastChunkMS: uxrecord.Unknown, EndMS: 10, MaxGapMS: uxrecord.Unknown, CachedTokens: uxrecord.Unknown,
		ReasoningTokens: uxrecord.Unknown, EngineQueueMS: uxrecord.Unknown, EnginePrefillMS: uxrecord.Unknown,
		EngineDecodeMS: uxrecord.Unknown,
	}
	for _, tc := range []struct {
		name       string
		edit       func(*uxrecord.Request)
		wantErr    string // error.type, "" for no error
		wantResult string
	}{
		{"upstream unreachable", func(r *uxrecord.Request) { r.Status, r.Outcome = 502, uxrecord.OutcomeUpstreamError }, "upstream_error", "error"},
		{"engine 500", func(r *uxrecord.Request) { r.Status = 500 }, "500", "error"},
		{"cut short", func(r *uxrecord.Request) { r.Status, r.Outcome = 200, uxrecord.OutcomeTruncated }, "truncated", "interrupted"},
		{"client left", func(r *uxrecord.Request) { r.Status, r.Outcome = 200, uxrecord.OutcomeClientClosed }, "", "client_closed"},
		{"bad request", func(r *uxrecord.Request) { r.Status = 400 }, "400", "error"},
		{"cut at max tokens", func(r *uxrecord.Request) { r.Status, r.FinishReason = 200, "length" }, "", "length"},
	} {
		r := base
		tc.edit(&r)
		sp := spanOf(r)
		got := map[string]string{}
		for _, a := range sp.Attributes {
			got[a.Key] = valueText(a.Value)
		}
		if got["error.type"] != tc.wantErr || got["gpuworks.request.result"] != tc.wantResult {
			t.Errorf("%s: error.type %q result %q", tc.name, got["error.type"], got["gpuworks.request.result"])
		}
		if (sp.Status.Code == statusCodeError) != (tc.wantErr != "") || (tc.wantErr != "" && sp.Status.Message != tc.wantResult) {
			t.Errorf("%s: status %+v", tc.name, sp.Status)
		}
		for _, key := range []string{"gen_ai.operation.name", "gen_ai.request.model", "gen_ai.usage.input_tokens", "gen_ai.response.time_to_first_chunk", "gpuworks.instance.uuid", "gpuworks.api_key.id", "gpuworks.stream.events",
			"gen_ai.request.max_tokens", "gen_ai.request.temperature", "gen_ai.request.top_p", "gen_ai.request.choice.count",
			"gen_ai.latency.time_in_queue", "gen_ai.latency.time_in_model_prefill", "gen_ai.latency.time_in_model_decode", "gen_ai.latency.time_in_model_inference"} {
			if _, ok := got[key]; ok {
				t.Errorf("%s: %s set without a value behind it", tc.name, key)
			}
		}
		if sp.Name != "/v1/rerank" || sp.ParentSpanID != "" || sp.TraceState != "" || sp.Flags != flagsHasRemote|1 {
			t.Errorf("%s: span %q parent %q flags %x", tc.name, sp.Name, sp.ParentSpanID, sp.Flags)
		}
		if len(sp.Events) != 1 || sp.Events[0].Name != "gpuworks.dispatch" {
			t.Errorf("%s: events %+v", tc.name, sp.Events)
		}
	}
	// Usage without a cache figure: tokens, no cache attribute.
	r := base
	r.Usage, r.PromptTokens, r.CompletionTokens = true, 10, 0
	for _, a := range spanOf(r).Attributes {
		if a.Key == "gen_ai.usage.cache_read.input_tokens" || a.Key == "gen_ai.usage.reasoning.output_tokens" {
			t.Errorf("%s set without the engine reporting it", a.Key)
		}
	}
}

func valueText(v anyValue) string {
	switch v.kind {
	case kindString:
		return v.s
	case kindInt:
		return strconv.FormatInt(v.i, 10)
	}
	return ""
}

func TestTracesURLAndValidate(t *testing.T) {
	for in, want := range map[string]string{
		"http://collector:4318":         "http://collector:4318/v1/traces",
		" https://otel.example.com/ ":   "https://otel.example.com/v1/traces",
		"https://otel.example.com/otlp": "https://otel.example.com/otlp/v1/traces",
		"http://c:4318/v1/traces":       "http://c:4318/v1/traces",
		"http://c:4318/v1/traces/":      "http://c:4318/v1/traces",
		"http://[::1]:4318":             "http://[::1]:4318/v1/traces",
	} {
		if got, err := TracesURL(in); err != nil || got != want {
			t.Errorf("TracesURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "collector:4318", "grpc://c:4317", "http://", "http://u:p@c:4318", "http://c:4318?x=1", "http://c/#f"} {
		if _, err := TracesURL(bad); err == nil {
			t.Errorf("TracesURL(%q) accepted", bad)
		}
	}
	ok := Config{Enabled: true, Endpoint: "http://c:4318", SampleRatio: 1, Headers: map[string]string{"Authorization": "Bearer x", "X-Scope-OrgID": "t1"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]Config{
		"ratio above 1":       {Enabled: true, Endpoint: "http://c", SampleRatio: 1.5},
		"negative ratio":      {Enabled: true, Endpoint: "http://c", SampleRatio: -0.1},
		"bad endpoint":        {Enabled: true, Endpoint: "c:4318", SampleRatio: 1},
		"bad header name":     {Enabled: true, Endpoint: "http://c", SampleRatio: 1, Headers: map[string]string{"Bad Name": "x"}},
		"empty header":        {Enabled: true, Endpoint: "http://c", SampleRatio: 1, Headers: map[string]string{"": "x"}},
		"reserved header":     {Enabled: true, Endpoint: "http://c", SampleRatio: 1, Headers: map[string]string{"content-encoding": "br"}},
		"header newline":      {Enabled: true, Endpoint: "http://c", SampleRatio: 1, Headers: map[string]string{"X-A": "a\r\nX-B: b"}},
		"disabled bad header": {Endpoint: "http://c", Headers: map[string]string{"Bad Name": "x"}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (Config{Endpoint: "half-typed"}).Validate(); err != nil {
		t.Errorf("a disabled half-typed endpoint was refused: %v", err)
	}
}

func fastOptions() Options {
	return Options{AllowPrivateHosts: []string{"127.0.0.1"}, BatchSize: 3, FlushEvery: 20 * time.Millisecond, Backoff: 5 * time.Millisecond, MaxRetryAfter: 50 * time.Millisecond, CloseWait: time.Second}
}

func newExporter(t *testing.T, c *fakeCollector, opts Options) *Exporter {
	t.Helper()
	opts.Resource = Resource{ServiceVersion: "test"}
	e := New(opts)
	t.Cleanup(e.Close)
	if c != nil {
		if err := e.Configure(Config{Enabled: true, Endpoint: c.server.URL, SampleRatio: 1, Headers: map[string]string{"Authorization": "Bearer t0k"}}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func record(t *testing.T, id string, sampled bool) uxrecord.Request {
	r := streamedChat(t)
	r.ID = id
	r.Trace.Flags = 0
	if sampled {
		r.Trace.Flags = tracecontext.FlagSampled
	}
	return r
}

// Sampled records are batched to the collector with the configured headers,
// gzipped; unsampled ones are not sent at all.
func TestTheExporterBatchesSampledRecords(t *testing.T) {
	c := newCollector(t)
	e := newExporter(t, c, fastOptions())
	for i, sampled := range []bool{true, false, true, true, true} {
		e.Observe(record(t, "r"+string(rune('0'+i)), sampled))
	}
	waitFor(t, "four spans", func() bool { return len(c.spans()) == 4 })
	if n := c.callCount(); n != 2 {
		t.Fatalf("posts = %d, want a full batch of 3 and a timed one of 1", n)
	}
	c.mu.Lock()
	h := c.header[0]
	c.mu.Unlock()
	if h.Get("Authorization") != "Bearer t0k" || h.Get("Content-Encoding") != "gzip" {
		t.Fatalf("headers = %v", h)
	}
	ids := []string{}
	for _, sp := range c.spans() {
		ids = append(ids, attrs(sp.Attributes)["gpuworks.request.id"])
	}
	if strings.Join(ids, ",") != "r0,r2,r3,r4" {
		t.Fatalf("spans = %v", ids)
	}
	waitFor(t, "status", func() bool { return e.Status().Exported == 4 })
	st := e.Status()
	if !st.Enabled || st.Endpoint != c.server.URL+"/v1/traces" || st.Failed != 0 || st.Dropped != 0 || st.LastError != "" || st.LastExportAt.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

// Off, nothing is sampled and nothing queued; a record without ids (written
// before traces existed) is never sent.
func TestADisabledExporterSamplesAndSendsNothing(t *testing.T) {
	e := newExporter(t, nil, fastOptions())
	id := tracecontext.NewTraceID(time.Now())
	if e.Sample(id) {
		t.Fatal("sampled while off")
	}
	e.Observe(record(t, "r", true))
	if st := e.Status(); st.Enabled || st.Queued != 0 || st.Dropped != 0 {
		t.Fatalf("status = %+v", st)
	}
	c := newCollector(t)
	if err := e.Configure(Config{Enabled: true, Endpoint: c.server.URL, SampleRatio: 0}); err != nil {
		t.Fatal(err)
	}
	if e.Sample(id) {
		t.Fatal("sampled at ratio 0")
	}
	if err := e.Configure(Config{Enabled: true, Endpoint: c.server.URL, SampleRatio: 1}); err != nil {
		t.Fatal(err)
	}
	if !e.Sample(id) {
		t.Fatal("not sampled at ratio 1")
	}
	r := record(t, "no-ids", true)
	r.Trace.Trace = tracecontext.TraceID{}
	e.Observe(r)
	e.Observe(record(t, "sent", true))
	waitFor(t, "a span", func() bool { return len(c.spans()) == 1 })
	if got := attrs(c.spans()[0].Attributes)["gpuworks.request.id"]; got != "sent" {
		t.Fatalf("sent %q", got)
	}
}

// A collector that says it is overloaded is retried after its Retry-After
// (capped), then the batch lands.
func TestRetryableAnswersAreRetried(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		c := newCollector(t, reply{status: status, body: `{"code":14,"message":"busy"}`, retryAfter: "120"})
		e := newExporter(t, c, fastOptions())
		start := time.Now()
		e.Observe(record(t, "r", true))
		waitFor(t, "export", func() bool { return e.Status().Exported == 1 })
		if took := time.Since(start); took < 50*time.Millisecond || took > 3*time.Second {
			t.Fatalf("%d: retried after %v, want the capped Retry-After (50ms)", status, took)
		}
		if c.callCount() != 2 || e.Status().Failed != 0 {
			t.Fatalf("%d: calls %d status %+v", status, c.callCount(), e.Status())
		}
	}
}

// Retries give up after Options.Retries; the spans count as failed and the
// reason is kept.
func TestRetriesGiveUp(t *testing.T) {
	busy := reply{status: http.StatusServiceUnavailable, body: `{"code":14,"message":"unavailable"}`}
	c := newCollector(t, busy, busy, busy)
	opts := fastOptions()
	opts.Retries = 2
	e := newExporter(t, c, opts)
	e.Observe(record(t, "r", true))
	waitFor(t, "failure", func() bool { return e.Status().Failed == 1 })
	st := e.Status()
	if c.callCount() != 3 || st.Exported != 0 || !strings.Contains(st.LastError, "503") || !strings.Contains(st.LastError, "unavailable") || st.LastErrorAt.IsZero() {
		t.Fatalf("calls %d status %+v", c.callCount(), st)
	}
}

// Anything else is final at once: no retry.
func TestFinalAnswersAreNotRetried(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError} {
		c := newCollector(t, reply{status: status, body: `{"code":16,"message":"no tenant"}`})
		e := newExporter(t, c, fastOptions())
		e.Observe(record(t, "r", true))
		waitFor(t, "failure", func() bool { return e.Status().Failed == 1 })
		if c.callCount() != 1 || !strings.Contains(e.Status().LastError, "no tenant") {
			t.Fatalf("%d: calls %d status %+v", status, c.callCount(), e.Status())
		}
	}
}

// A collector nobody listens on is retried, then given up.
func TestAnUnreachableCollectorFails(t *testing.T) {
	c := newCollector(t)
	url := c.server.URL
	c.server.Close()
	e := newExporter(t, nil, fastOptions())
	if err := e.Configure(Config{Enabled: true, Endpoint: url, SampleRatio: 1}); err != nil {
		t.Fatal(err)
	}
	e.Observe(record(t, "r", true))
	waitFor(t, "failure", func() bool { return e.Status().Failed == 1 })
	if !strings.Contains(e.Status().LastError, "post "+url) {
		t.Fatalf("status = %+v", e.Status())
	}
}

// A partial success counts what the collector took and what it rejected.
func TestAPartialSuccessCountsTheRejected(t *testing.T) {
	c := newCollector(t, reply{status: http.StatusOK, body: `{"partialSuccess":{"rejectedSpans":"2","errorMessage":"span too old"}}`})
	e := newExporter(t, c, fastOptions())
	for _, id := range []string{"a", "b", "c"} {
		e.Observe(record(t, id, true))
	}
	waitFor(t, "accounting", func() bool { st := e.Status(); return st.Exported+st.Failed == 3 })
	st := e.Status()
	if st.Exported != 1 || st.Failed != 2 || !strings.Contains(st.LastError, "span too old") {
		t.Fatalf("status = %+v", st)
	}
}

// A full queue drops the record and counts it; Observe does not wait.
func TestAFullQueueDropsAndCounts(t *testing.T) {
	c := newCollector(t)
	c.hold = make(chan struct{})
	opts := fastOptions()
	opts.Queue, opts.BatchSize = 2, 1
	e := newExporter(t, c, opts)
	start := time.Now()
	for i := 0; i < 50; i++ {
		e.Observe(record(t, "r", true))
	}
	if took := time.Since(start); took > 100*time.Millisecond {
		t.Fatalf("Observe blocked for %v", took)
	}
	if st := e.Status(); st.Dropped < 40 {
		t.Fatalf("status = %+v", st)
	}
	close(c.hold)
}

// A new configuration takes effect for the next batch; an invalid one is
// refused and the running one kept; turning export off discards what was
// queued under it.
func TestConfigureSwitchesAtOnce(t *testing.T) {
	first, second := newCollector(t), newCollector(t)
	e := newExporter(t, first, fastOptions())
	e.Observe(record(t, "to-first", true))
	waitFor(t, "first", func() bool { return len(first.spans()) == 1 })
	if err := e.Configure(Config{Enabled: true, Endpoint: "not a url", SampleRatio: 1}); err == nil {
		t.Fatal("an invalid configuration was taken")
	}
	if err := e.Configure(Config{Enabled: true, Endpoint: second.server.URL, SampleRatio: 1}); err != nil {
		t.Fatal(err)
	}
	e.Observe(record(t, "to-second", true))
	waitFor(t, "second", func() bool { return len(second.spans()) == 1 })
	if len(first.spans()) != 1 {
		t.Fatal("the old endpoint got more")
	}

	second.hold = make(chan struct{})
	opts := fastOptions()
	opts.FlushEvery = time.Hour
	off := newExporter(t, second, opts)
	off.Observe(record(t, "queued", true))
	if err := off.Configure(Config{}); err != nil {
		t.Fatal(err)
	}
	off.Close()
	close(second.hold)
	if st := off.Status(); st.Enabled || st.Exported != 0 || len(second.spans()) != 1 {
		t.Fatalf("status %+v spans %d", st, len(second.spans()))
	}
}

// Close sends what is queued; with a collector that hangs it gives up after
// CloseWait and counts the rest.
func TestCloseDrainsWithinItsDeadline(t *testing.T) {
	c := newCollector(t)
	opts := fastOptions()
	opts.FlushEvery = time.Hour
	e := newExporter(t, c, opts)
	e.Observe(record(t, "a", true))
	e.Observe(record(t, "b", true))
	e.Close()
	if len(c.spans()) != 2 || e.Status().Exported != 2 {
		t.Fatalf("spans %d status %+v", len(c.spans()), e.Status())
	}
	e.Observe(record(t, "late", true))
	if e.Status().Dropped != 1 {
		t.Fatalf("a record after Close: %+v", e.Status())
	}

	hung := newCollector(t)
	hung.hold = make(chan struct{})
	defer close(hung.hold)
	opts.CloseWait = 100 * time.Millisecond
	slow := newExporter(t, hung, opts)
	slow.Observe(record(t, "a", true))
	start := time.Now()
	slow.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Close took %v", took)
	}
	if st := slow.Status(); st.Exported != 0 || st.Failed+st.Dropped != 1 {
		t.Fatalf("status = %+v", st)
	}
}

// Test posts one span to a configuration that need not be the running one,
// says what the collector answered and leaves the counters alone.
func TestTestSendsOneSpan(t *testing.T) {
	c := newCollector(t)
	e := newExporter(t, nil, fastOptions())
	res, err := e.Test(context.Background(), Config{Endpoint: c.server.URL, Headers: map[string]string{"X-Scope-OrgID": "lab"}})
	if err != nil {
		t.Fatal(err)
	}
	spans := c.spans()
	if len(spans) != 1 || spans[0].Name != "gpuworks test span" || spans[0].TraceID != res.TraceID || res.URL != c.server.URL+"/v1/traces" {
		t.Fatalf("spans %+v result %+v", spans, res)
	}
	if attrs(spans[0].Attributes)["gpuworks.test"] != "true" || c.header[0].Get("X-Scope-OrgID") != "lab" {
		t.Fatalf("span attributes %v headers %v", spans[0].Attributes, c.header[0])
	}
	if st := e.Status(); st.Enabled || st.Exported != 0 {
		t.Fatalf("Test changed the exporter: %+v", st)
	}

	refusing := newCollector(t, reply{status: http.StatusUnauthorized, body: `{"code":16,"message":"bad token"}`})
	if _, err := e.Test(context.Background(), Config{Endpoint: refusing.server.URL}); err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err = %v", err)
	}
	partial := newCollector(t, reply{status: http.StatusOK, body: `{"partialSuccess":{"rejectedSpans":"1","errorMessage":"no"}}`})
	if _, err := e.Test(context.Background(), Config{Endpoint: partial.server.URL}); err == nil {
		t.Fatal("a rejected test span passed")
	}
	if _, err := e.Test(context.Background(), Config{Endpoint: "nope"}); err == nil {
		t.Fatal("an invalid endpoint passed")
	}
	if st := e.Status(); st.Failed != 0 || st.LastError != "" {
		t.Fatalf("Test changed the counters: %+v", st)
	}
}

func BenchmarkEncodeBatch(b *testing.B) {
	t := &testing.T{}
	records := make([]uxrecord.Request, DefaultBatchSize)
	for i := range records {
		records[i] = streamedChat(t)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, _, err := encodeRecords(Resource{ServiceVersion: "v"}, records); err != nil {
			b.Fatal(err)
		}
	}
}

// Values that come from clients (a request id, a model name, a tracestate)
// are escaped: the batch stays valid JSON and decodes to what was sent.
func TestClientTextIsEscaped(t *testing.T) {
	r := streamedChat(t)
	r.ID = "id \"quoted\" \\ back\nline <b>&amp; é   \x01"
	r.Model = "模型/qwen"
	r.Trace.State = `k="v"`
	raw, _, err := encodeRecords(Resource{Name: "实验室"}, []uxrecord.Request{r})
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) {
		t.Fatalf("invalid JSON: %s", raw)
	}
	var back wireRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	sp := back.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if got := attrs(sp.Attributes)["gpuworks.request.id"]; got != r.ID || sp.Name != "chat 模型/qwen" || sp.TraceState != `k="v"` {
		t.Fatalf("decoded id %q name %q state %q", got, sp.Name, sp.TraceState)
	}
	if got := attrs(back.ResourceSpans[0].Resource.Attributes)["gpuworks.control_plane.name"]; got != "实验室" {
		t.Fatalf("resource name %q", got)
	}
	for _, f := range []float64{0, 0.12, 1e-7, 123456789, 2e21} {
		var v float64
		if err := json.Unmarshal(appendFloat(nil, f), &v); err != nil || v != f {
			t.Fatalf("float %v wrote %s", f, appendFloat(nil, f))
		}
	}
	if string(appendFloat(nil, math.NaN())) != "0" || string(appendFloat(nil, math.Inf(1))) != "0" {
		t.Fatal("non-finite floats")
	}
}

// A refused configuration names the field at fault.
func TestValidateNamesTheField(t *testing.T) {
	for want, c := range map[string]Config{
		"sample_ratio": {SampleRatio: 2},
		"header":       {Headers: map[string]string{"Bad Name": ""}},
		"endpoint":     {Enabled: true, Endpoint: "nope", SampleRatio: 1},
	} {
		var ce *ConfigError
		if err := c.Validate(); !errors.As(err, &ce) || ce.Field != want {
			t.Errorf("%s: err = %v", want, err)
		}
	}
	var ce *ConfigError
	if err := (Config{Headers: map[string]string{"Host": "x"}}).Validate(); !errors.As(err, &ce) || ce.Header != "Host" {
		t.Errorf("header name not reported: %v", err)
	}
}

// An engine that reports its prefill but not its queue (TensorFold,
// Strata): the prefill is under vLLM's name, no queue, no inference total
// without a decode; the rest of the wait stays the gateway's "waiting". A
// temperature of 0 the client set is a value, not an absence.
func TestSpanOfAnEngineThatReportsOnlyItsPrefill(t *testing.T) {
	r := streamedChat(t)
	r.EngineQueueMS, r.EngineDecodeMS = uxrecord.Unknown, uxrecord.Unknown
	r.Sampling = uxrecord.Sampling{Temperature: 0, HasTemperature: true}
	got := map[string]float64{}
	for _, a := range spanOf(r).Attributes {
		got[a.Key] = a.Value.f
	}
	if v, ok := got["gen_ai.latency.time_in_model_prefill"]; !ok || v != 0.08 {
		t.Fatalf("prefill = %v %v", v, ok)
	}
	for _, key := range []string{"gen_ai.latency.time_in_queue", "gen_ai.latency.time_in_model_decode", "gen_ai.latency.time_in_model_inference", "gen_ai.request.max_tokens", "gpuworks.phase.queue_ms", "gpuworks.phase.prefill_ms"} {
		if _, ok := got[key]; ok {
			t.Errorf("%s set", key)
		}
	}
	if _, ok := got["gpuworks.phase.waiting_ms"]; !ok {
		t.Error("no waiting phase")
	}
	if v, ok := got["gen_ai.request.temperature"]; !ok || v != 0 {
		t.Errorf("temperature = %v %v", v, ok)
	}
}

// A PD request the gateway waited for: its span names the prefill instance,
// and the prefill stage is a CLIENT child span timed as the stage, carrying
// the prefill instance's own queue and prefill under vLLM's names. Both
// count as exported spans.
func TestAPDRequestExportsItsPrefillStage(t *testing.T) {
	r := streamedChat(t)
	r.DispatchMS = 816
	r.PD = &uxrecord.PDStage{PrefillInstance: "p-1", PrefillMS: 795, PrefillQueueMS: 2, PrefillComputeMS: 640, StartMS: 20}
	copy(r.PD.SpanID[:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	raw, n, err := encodeRecords(Resource{}, []uxrecord.Request{r})
	if err != nil || n != 2 {
		t.Fatalf("spans %d err %v", n, err)
	}
	var back wireRequest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	spans := back.ResourceSpans[0].ScopeSpans[0].Spans
	parent, child := spans[0], spans[1]
	if attrs(parent.Attributes)["gpuworks.pd.prefill_instance"] != "p-1" || attrs(parent.Attributes)["gpuworks.phase.pd_prefill_ms"] != "795" {
		t.Fatalf("parent attributes %v", attrs(parent.Attributes))
	}
	if child.Name != "pd.prefill" || child.Kind != spanKindClient || child.SpanID != "0102030405060708" || child.ParentSpanID != parent.SpanID ||
		child.TraceID != parent.TraceID || child.Start != "1760500000020000000" || child.End != "1760500000815000000" {
		t.Fatalf("child = %+v", child)
	}
	ca := attrs(child.Attributes)
	if ca["gpuworks.pd.prefill_instance"] != "p-1" || ca["gen_ai.latency.time_in_queue"] != "0.002" || ca["gen_ai.latency.time_in_model_prefill"] != "0.64" {
		t.Fatalf("child attributes %v", ca)
	}

	// No stage time (concurrent handoff), no child span; a fallback is on
	// the request's span.
	r.PD = &uxrecord.PDStage{PrefillInstance: "p-1", PrefillMS: uxrecord.Unknown, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: uxrecord.Unknown}
	if _, n, _ := encodeRecords(Resource{}, []uxrecord.Request{r}); n != 1 {
		t.Fatalf("concurrent handoff spans = %d", n)
	}
	r.PD = &uxrecord.PDStage{PrefillMS: 3, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: uxrecord.Unknown, Fallback: "prefill_5xx"}
	raw, n, _ = encodeRecords(Resource{}, []uxrecord.Request{r})
	_ = json.Unmarshal(raw, &back)
	if n != 1 || attrs(back.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes)["gpuworks.pd.fallback"] != "prefill_5xx" {
		t.Fatalf("fallback spans %d", n)
	}
}

// The exporter counts spans, not requests: a PD request exports two.
func TestThePDStageSpanIsCountedAndSent(t *testing.T) {
	c := newCollector(t)
	e := newExporter(t, c, fastOptions())
	r := record(t, "pd", true)
	r.PD = &uxrecord.PDStage{PrefillInstance: "p-1", PrefillMS: 100, PrefillQueueMS: uxrecord.Unknown, PrefillComputeMS: uxrecord.Unknown, StartMS: 1}
	r.PD.SpanID[7] = 9
	e.Observe(r)
	waitFor(t, "two spans", func() bool { return e.Status().Exported == 2 })
	if len(c.spans()) != 2 {
		t.Fatalf("collector got %d spans", len(c.spans()))
	}
}
