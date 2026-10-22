package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Only SGLang with its cache report on omits a zero cache figure; the switch
// comes from the stored launch configuration, or the user's arguments for a
// deployment stored before the dictionary described it.
func TestCacheReportOmitsZero(t *testing.T) {
	for _, tc := range []struct {
		dep  store.Deployment
		want bool
	}{
		{store.Deployment{Engine: "sglang", QuickParams: `{"enable_cache_report":true}`}, true},
		{store.Deployment{Engine: "SGLang ", QuickParams: `{"enable_cache_report":true,"dtype":"auto"}`}, true},
		{store.Deployment{Engine: "sglang", QuickParams: `{"enable_cache_report":false}`, ExtraArgs: "--enable-cache-report"}, false},
		{store.Deployment{Engine: "sglang", ExtraArgs: "--enable-cache-report --log-level info"}, true},
		{store.Deployment{Engine: "sglang", QuickParams: `{}`}, false},
		{store.Deployment{Engine: "vllm", QuickParams: `{"enable_cache_report":true}`}, false},
		{store.Deployment{Engine: "callable", ExtraArgs: "--enable-cache-report"}, false},
	} {
		if got := cacheReportOmitsZero(&tc.dep); got != tc.want {
			t.Errorf("%+v: got %v, want %v", tc.dep, got, tc.want)
		}
	}
	if cacheReportOmitsZero(nil) {
		t.Error("nil deployment")
	}
}

// How a forwarded reply ended, as its record says: a client that left
// cancels the request's context (the read of the upstream body fails, or the
// write to the client does); anything else that cuts the copy short is a
// truncation.
func TestCopyObservationOutcome(t *testing.T) {
	for _, tc := range []struct {
		name     string
		readErr  error
		writeErr error
		want     uxrecord.Outcome
	}{
		{"complete", io.EOF, nil, uxrecord.OutcomeOK},
		{"client left while reading", context.Canceled, nil, uxrecord.OutcomeClientClosed},
		{"client left while writing", nil, errors.New("broken pipe"), uxrecord.OutcomeClientClosed},
		{"upstream cut", io.ErrUnexpectedEOF, nil, uxrecord.OutcomeTruncated},
		{"upstream deadline", context.DeadlineExceeded, nil, uxrecord.OutcomeTruncated},
	} {
		got := uxrecord.Outcome(255)
		o := &copyObservation{log: slog.New(slog.NewTextHandler(io.Discard, nil)), declaredLen: -1, status: 200}
		o.onFinish = func(_ int, _ time.Duration, _ responseStats, outcome uxrecord.Outcome) { got = outcome }
		o.writeErr = tc.writeErr
		o.finish(tc.readErr)
		if got != tc.want {
			t.Errorf("%s: outcome %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A client that leaves once it has the reply's last event received the whole
// reply: the record says so and keeps the reply's usage. Leaving before that
// event is still a client that closed, and so is a failed write of it.
func TestCopyObservationOutcomeAfterTheLastEvent(t *testing.T) {
	last := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}` + "\n\n"
	middle := `data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}]}` + "\n\n"
	for _, tc := range []struct {
		name      string
		body      string
		writeErr  error
		want      uxrecord.Outcome
		wantUsage bool
	}{
		{"left after the last event", middle + last, nil, uxrecord.OutcomeOK, true},
		{"left mid-reply", middle, nil, uxrecord.OutcomeClientClosed, false},
		{"the last event never reached the client", middle + last, errors.New("broken pipe"), uxrecord.OutcomeClientClosed, true},
	} {
		got, gotStats := uxrecord.Outcome(255), responseStats{}
		o := &copyObservation{log: slog.New(slog.NewTextHandler(io.Discard, nil)), declaredLen: -1, status: 200}
		o.response = newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
		o.response.observe([]byte(tc.body))
		o.onFinish = func(_ int, _ time.Duration, stats responseStats, outcome uxrecord.Outcome) {
			got, gotStats = outcome, stats
		}
		o.writeErr = tc.writeErr
		o.finish(context.Canceled)
		if got != tc.want || gotStats.usage != tc.wantUsage {
			t.Errorf("%s: outcome %v usage %v, want %v %v", tc.name, got, gotStats.usage, tc.want, tc.wantUsage)
		}
	}
}
