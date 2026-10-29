package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The server's own requests that act for a user rather than for the platform
// (the Batch API, internal/server/batch) take the path a client's request
// takes, with no connection in between: the model is looked up, a request for
// a deployment put to sleep is held while it wakes, and the request is
// routed, retried across replicas, counted and recorded, so it is the
// deployment's traffic, unlike a probe (probe.go).

// MaxInProcessReply is the largest response Do keeps.
const MaxInProcessReply = 16 << 20

// inProcessReplyLimit is MaxInProcessReply; a test lowers it.
var inProcessReplyLimit = MaxInProcessReply

// InProcessReply is what an in-process request came to.
type InProcessReply struct {
	// Status is the response's status, 0 when none came.
	Status int
	// RequestID is the gateway's id of the request.
	RequestID string
	Body      []byte
	// RetryAfter is the delay the response asked for.
	RetryAfter time.Duration
	// TooLarge: the response was longer than MaxInProcessReply, so Body is cut.
	TooLarge bool
	// Err is the context's error when ctx ended before the response did.
	Err error
}

// Identify names the caller of a request from its API key, as the /v1 entry
// does: with no key configured yet every request is allowed, anonymously.
// False when the key is missing or invalid.
func (p *Proxy) Identify(r *http.Request) (uxrecord.Caller, bool) {
	caller := uxrecord.Caller{Source: uxrecord.SourceAPI, ReceivedMS: time.Now().UnixMilli()}
	if !p.store.HasAnyAPIKey() {
		return caller, true
	}
	key := auth.BearerToken(r.Header.Get("Authorization"))
	if key == "" {
		return caller, false
	}
	identity, ok := p.store.ResolveAPIKey(key)
	if !ok {
		return caller, false
	}
	caller.APIKeyID, caller.UserID = identity.KeyID, identity.UserID
	return caller, true
}

// Do posts body to path (a /v1 path) as caller's request and returns the
// response. ctx bounds it, the wake hold included.
func (p *Proxy) Do(ctx context.Context, caller uxrecord.Caller, path string, body []byte) InProcessReply {
	caller.Source = uxrecord.SourceAPI
	caller.ReceivedMS = time.Now().UnixMilli()
	req, err := http.NewRequestWithContext(uxrecord.WithCaller(ctx, caller), http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return InProcessReply{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	w := &replyWriter{header: http.Header{}, limit: inProcessReplyLimit}
	p.serveAuthenticated(w, req)
	out := InProcessReply{Status: w.status, Body: w.body.Bytes(), TooLarge: w.tooLarge, RequestID: w.header.Get("X-Request-ID")}
	if out.Status == 0 && w.wrote {
		out.Status = http.StatusOK
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(w.header.Get("Retry-After"))); err == nil && secs > 0 {
		out.RetryAfter = time.Duration(secs) * time.Second
	}
	if ctx.Err() != nil {
		out.Err = ctx.Err()
	}
	return out
}

var errReplyTooLarge = errors.New("response too large")

// replyWriter keeps a response in memory up to a limit.
type replyWriter struct {
	header   http.Header
	status   int
	wrote    bool
	body     bytes.Buffer
	limit    int
	tooLarge bool
}

func (w *replyWriter) Header() http.Header { return w.header }

func (w *replyWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *replyWriter) Write(b []byte) (int, error) {
	w.wrote = true
	if w.tooLarge {
		return 0, errReplyTooLarge
	}
	if w.body.Len()+len(b) > w.limit {
		w.tooLarge = true
		return 0, errReplyTooLarge
	}
	return w.body.Write(b)
}

func (w *replyWriter) Flush() {}
