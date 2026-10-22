package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// obsCtxKey carries the per-request copyObservation through the reverse-proxy
// request context so the ModifyResponse hook (which wraps the upstream body)
// and the response-writer wrapper (which counts client bytes) share one struct.
type obsCtxKey int

const obsKey obsCtxKey = 0

// copyObservation accumulates everything needed to detect and locate a
// mid-body truncation: the request id, the upstream's declared framing, the
// bytes actually streamed to the client, and the terminal read/write errors.
//
// httputil.ReverseProxy only invokes ErrorHandler before the response headers
// are written; once headers are out and the tunnel dies mid-copy the client
// silently receives a short body. This struct is populated across the copy and
// logged exactly once when the upstream body is closed, so a single greppable
// line names the layer that truncated.
type copyObservation struct {
	log       *slog.Logger
	requestID string
	method    string
	path      string
	started   time.Time

	// declaredLen is the upstream Content-Length, or -1 when the response is
	// chunked / has no declared length. status is the upstream status code.
	declaredLen int64
	status      int

	// onFinish, when set, is invoked exactly once from finish() with the
	// upstream status, the request's total duration and what the reply
	// contributed. It is how proxy request accounting (accounting.go) learns
	// that a dispatched request completed, without this package knowing
	// anything about accounting.
	onFinish func(status int, duration time.Duration, stats responseStats, outcome uxrecord.Outcome)

	// response observes the reply's first token and usage
	// (usage_observer.go); nil when the reply is not one to observe. Set in
	// ModifyResponse and fed by countingBody on the copying goroutine.
	response *responseObserver
	// reading and live are handed to the response observer: how the
	// deployment's replies are read and the request's in-flight entry.
	// injected says the gateway asked for usage, so the usage event it caused
	// is stripped.
	reading  replyReading
	live     *liveRequest
	injected bool
	// pdCached is the prefill instance's prompt-cache figure, which the
	// decode reply reports instead of its own (cached_rewrite.go); only a
	// PD request whose prefill instance stated one has it.
	pdCached      uint64
	pdCachedKnown bool
	// firstByte is when the upstream's headers arrived. upstreamFailed and
	// clientGone are set by the ErrorHandler when no reply arrived at all.
	firstByte      time.Time
	upstreamFailed bool
	clientGone     bool

	mu       sync.Mutex
	written  int64
	writeErr error
	logged   bool
}

// recordWrite accounts for bytes handed to the client and the first write error.
func (o *copyObservation) recordWrite(n int, err error) {
	o.mu.Lock()
	o.written += int64(n)
	if err != nil && o.writeErr == nil {
		o.writeErr = err
	}
	o.mu.Unlock()
}

// finish logs the copy outcome once. readErr is the terminal error observed
// reading the upstream body (nil or io.EOF means a clean end).
func (o *copyObservation) finish(readErr error) {
	o.mu.Lock()
	if o.logged {
		o.mu.Unlock()
		return
	}
	o.logged = true
	written := o.written
	writeErr := o.writeErr
	o.mu.Unlock()

	copyErr := readErr
	if copyErr != nil && errors.Is(copyErr, io.EOF) {
		copyErr = nil
	}

	// A declared Content-Length that does not match the delivered bytes is a
	// truncation even when neither side surfaced an explicit error.
	lengthMismatch := o.declaredLen >= 0 && written != o.declaredLen
	truncated := copyErr != nil || writeErr != nil || lengthMismatch
	duration := time.Since(o.started)

	// Accounting is classified by the upstream status the client was told,
	// even when the copy itself was truncated or the client aborted
	// mid-stream: a client that opened an SSE stream and walked away still
	// received (and should be counted under) the 200 the upstream sent.
	stats := o.response.stats(!truncated)
	outcome := uxrecord.OutcomeOK
	switch {
	// A client that leaves once it has the reply's last event, before the
	// stream's terminator, received the whole reply.
	case stats.ended && writeErr == nil && errors.Is(copyErr, context.Canceled):
		truncated = false
	// A client that leaves cancels the request's context, so the read of the
	// upstream body usually fails before any write to the client does.
	case o.upstreamFailed && o.clientGone, writeErr != nil, errors.Is(copyErr, context.Canceled):
		outcome = uxrecord.OutcomeClientClosed
	case o.upstreamFailed:
		outcome = uxrecord.OutcomeUpstreamError
	case truncated:
		outcome = uxrecord.OutcomeTruncated
	}
	if o.onFinish != nil {
		o.onFinish(o.status, duration, stats, outcome)
	}

	attrs := []any{
		"request_id", o.requestID,
		"method", o.method,
		"path", o.path,
		"upstream_status", o.status,
		"declared_length", o.declaredLen,
		"copied_bytes", written,
		"duration_ms", duration.Seconds() * 1000,
	}
	if copyErr != nil {
		attrs = append(attrs, "copy_error", copyErr.Error())
	}
	if writeErr != nil {
		attrs = append(attrs, "write_error", writeErr.Error())
	}
	if truncated {
		o.log.Warn("proxy response body truncated", attrs...)
		return
	}
	o.log.Debug("proxy response body copied", attrs...)
}

// countingResponseWriter counts bytes written to the client and captures the
// first client write error, delegating everything else (Flush, Hijack) so SSE
// streaming and WebSocket upgrades keep working.
type countingResponseWriter struct {
	http.ResponseWriter
	obs *copyObservation
	// firstByte, when set, runs before the first byte reaches the client.
	firstByte func()
}

func (w *countingResponseWriter) WriteHeader(status int) {
	w.fireFirstByte()
	w.ResponseWriter.WriteHeader(status)
}

func (w *countingResponseWriter) fireFirstByte() {
	if w.firstByte != nil {
		w.firstByte()
		w.firstByte = nil
	}
}

func (w *countingResponseWriter) Write(p []byte) (int, error) {
	w.fireFirstByte()
	n, err := w.ResponseWriter.Write(p)
	w.obs.recordWrite(n, err)
	return n, err
}

// Unwrap lets net/http.ResponseController reach the underlying Flusher/Hijacker.
func (w *countingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *countingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *countingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errHijackUnsupported
}

var errHijackUnsupported = errors.New("proxy: underlying ResponseWriter does not support hijacking")

// countingBody wraps the upstream response body, recording the terminal read
// error and closing exactly when the reverse proxy finishes the copy so the
// observation can be logged with final byte counts.
type countingBody struct {
	inner   io.ReadCloser
	obs     *copyObservation
	readErr error
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	b.obs.response.observe(p[:n])
	if err != nil && !errors.Is(err, io.EOF) {
		b.readErr = err
	}
	return n, err
}

func (b *countingBody) Close() error {
	err := b.inner.Close()
	b.obs.finish(b.readErr)
	return err
}
