package traceexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tracecontext"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// maxReplyBytes bounds how much of a collector's reply is read.
const maxReplyBytes = 64 << 10

// errRetryable marks a failure worth trying again: the collector could not
// be reached, or said it is overloaded or unavailable.
type errRetryable struct {
	err   error
	after time.Duration // the collector's Retry-After, 0 when none
}

func (e *errRetryable) Error() string { return e.err.Error() }
func (e *errRetryable) Unwrap() error { return e.err }

// post sends one batch with the configuration of the moment, retrying what is
// retryable, and accounts the spans. A batch sent while export is off is
// discarded: it was queued under a configuration the operator has since
// turned off.
func (e *Exporter) post(ctx context.Context, records []uxrecord.Request) {
	a := e.cfg.Load()
	if a == nil {
		return
	}
	body, spans, err := encodeRecords(e.opts.Resource, records)
	if err != nil {
		e.failed.Add(uint64(len(records)))
		e.noteError("encode spans: " + err.Error())
		return
	}
	n := uint64(spans)
	wait := e.opts.Backoff
	for attempt := 0; ; attempt++ {
		partial, err := e.send(ctx, a, body)
		if err == nil {
			rejected := min(partial.rejected, n)
			e.exported.Add(n - rejected)
			e.failed.Add(rejected)
			e.noteExport()
			if partial.rejected > 0 || partial.message != "" {
				e.noteError(fmt.Sprintf("collector rejected %d spans: %s", partial.rejected, partial.message))
			}
			return
		}
		var retry *errRetryable
		if !errors.As(err, &retry) || attempt >= e.opts.Retries || ctx.Err() != nil {
			e.failed.Add(n)
			e.noteError(err.Error())
			return
		}
		pause := wait
		if retry.after > 0 {
			pause = min(retry.after, e.opts.MaxRetryAfter)
		}
		wait *= 2
		select {
		case <-ctx.Done():
			e.failed.Add(n)
			e.noteError(err.Error())
			return
		case <-time.After(pause):
		}
	}
}

// exportReply is the part of ExportTraceServiceResponse the exporter reads.
type exportReply struct {
	PartialSuccess *struct {
		RejectedSpans json.Number `json:"rejectedSpans"`
		ErrorMessage  string      `json:"errorMessage"`
	} `json:"partialSuccess"`
}

// partialSuccess is what a 2xx reply said it did not take.
type partialSuccess struct {
	rejected uint64
	message  string
}

// send posts one gzipped export request; it touches no counters. A 2xx is
// success, with what a partial success rejected; 429, 502, 503, 504 and a
// failed connection are retryable; anything else is final.
func (e *Exporter) send(ctx context.Context, a *active, payload []byte) (partialSuccess, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		return partialSuccess{}, err
	}
	if err := zw.Close(); err != nil {
		return partialSuccess{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, &buf)
	if err != nil {
		return partialSuccess{}, err
	}
	for k, v := range a.cfg.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := e.opts.Client.Do(req)
	if err != nil {
		return partialSuccess{}, &errRetryable{err: fmt.Errorf("post %s: %w", a.url, err)}
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxReplyBytes))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		var r exportReply
		if json.Unmarshal(reply, &r) == nil && r.PartialSuccess != nil {
			n, _ := strconv.ParseUint(r.PartialSuccess.RejectedSpans.String(), 10, 64)
			return partialSuccess{rejected: n, message: r.PartialSuccess.ErrorMessage}, nil
		}
		return partialSuccess{}, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == http.StatusGatewayTimeout:
		return partialSuccess{}, &errRetryable{err: fmt.Errorf("collector answered %d: %s", resp.StatusCode, trimBody(reply)), after: retryAfter(resp.Header.Get("Retry-After"))}
	default:
		return partialSuccess{}, fmt.Errorf("collector answered %d: %s", resp.StatusCode, trimBody(reply))
	}
}

// retryAfter reads a Retry-After of delay seconds; a date or garbage is
// none.
func retryAfter(v string) time.Duration {
	secs, err := strconv.Atoi(v)
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs) * time.Second
}

// TestResult is what a test export did.
type TestResult struct {
	// TraceID is the test span's trace, to look up in the tracing backend.
	TraceID string
	// URL is where the span was posted.
	URL string
}

// Test posts one span named "gpuworks test span" to cfg's endpoint with its
// headers, once, without retries, whether or not cfg is enabled, and says
// whether the collector accepted it. It neither uses nor changes the
// exporter's configuration or counters.
func (e *Exporter) Test(ctx context.Context, cfg Config) (TestResult, error) {
	cfg.Enabled = true
	if err := cfg.Validate(); err != nil {
		return TestResult{}, err
	}
	u, _ := TracesURL(cfg.Endpoint)
	now := e.opts.Now()
	sp := tracecontext.Span{Trace: tracecontext.NewTraceID(now), ID: tracecontext.NewSpanID(), Flags: tracecontext.FlagSampled | tracecontext.FlagRandom}
	payload, err := encode(e.opts.Resource, []span{{
		TraceID: sp.Trace.String(), SpanID: sp.ID.String(), Flags: flagsHasRemote | 1,
		Name: "gpuworks test span", Kind: spanKindServer,
		Start: nanos(now.UnixMilli()), End: nanos(now.UnixMilli() + 1),
		Attributes: []attribute{{"gpuworks.test", boolean(true)}},
	}})
	if err != nil {
		return TestResult{}, err
	}
	out := TestResult{TraceID: sp.Trace.String(), URL: u}
	partial, err := e.send(ctx, &active{cfg: cfg, url: u}, payload)
	if err == nil && partial.rejected > 0 {
		err = fmt.Errorf("the collector rejected the test span: %s", partial.message)
	}
	return out, err
}
