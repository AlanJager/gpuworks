package pluginhost

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Host API rate limits per plugin (docs/dev/extensions.md): every call,
// and inventory pages on top.
const (
	hostRatePerSecond  = 20
	hostRateBurst      = 100
	hostPagesPerSecond = 2
	hostPagesBurst     = 4
)

// hostConn serves the host API on one plugin's host connection. Everything
// it answers is for the owner bound at the handshake: no request names
// one.
type hostConn struct {
	id        string
	lifecycle string
	state     StateHost
	reads     Reads
	log       *slog.Logger
	limiter   *tokenBucket
	// pages bounds inventory pages on top of limiter, so a plugin cannot
	// page through a large fleet over and over.
	pages *tokenBucket
	// owner is set and ready closed when the handshake is accepted;
	// refused is set and ready closed when it is not.
	owner   atomic.Pointer[Owner]
	refused atomic.Bool
	ready   chan struct{}
	settle  sync.Once
}

func newHostConn(spec *Spec, state StateHost, reads Reads, log *slog.Logger) *hostConn {
	return &hostConn{id: spec.ID, lifecycle: spec.Lifecycle, state: state, reads: reads, log: log,
		limiter: newTokenBucket(hostRatePerSecond, hostRateBurst, time.Now),
		pages:   newTokenBucket(hostPagesPerSecond, hostPagesBurst, time.Now), ready: make(chan struct{})}
}

// accept binds the owner once the handshake is accepted; reject makes
// every waiting and later call answer PLUGIN_HOST_NOT_READY.
func (h *hostConn) accept(o Owner) {
	h.settle.Do(func() { h.owner.Store(&o); close(h.ready) })
}

func (h *hostConn) reject() {
	h.settle.Do(func() { h.refused.Store(true); close(h.ready) })
}

func (h *hostConn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow() {
		e := Refuse(extapi.ReasonBusy, messages.ErrorPluginHostRateLimited, "the plugin calls the host API too fast",
			map[string]any{"limit": hostRatePerSecond})
		e.Wire.RetryAfterMS = 1000 / hostRatePerSecond
		writeWire(w, e)
		return
	}
	switch {
	case r.URL.Path == extapi.PathHostHealth:
		if r.Method != http.MethodGet {
			writeWire(w, methodNotServed(r))
			return
		}
		writeJSON(w, http.StatusOK, extapi.Health{Status: extapi.HealthOK})
	case strings.HasPrefix(r.URL.Path, extapi.PathHostState):
		if r.Method != http.MethodPut && r.Method != http.MethodPatch {
			writeWire(w, methodNotServed(r))
			return
		}
		h.push(w, r, strings.TrimPrefix(r.URL.Path, extapi.PathHostState))
	case r.URL.Path == extapi.PathHostNodeIdentity:
		if r.Method != http.MethodPost {
			writeWire(w, methodNotServed(r))
			return
		}
		h.nodeIdentity(w, r)
	case r.URL.Path == extapi.PathHostNodes:
		if r.Method != http.MethodGet {
			writeWire(w, methodNotServed(r))
			return
		}
		h.nodes(w, r)
	default:
		writeWire(w, Refuse(extapi.ReasonNotFound, messages.ErrorPluginHostRouteUnknown, "the host API has no such endpoint",
			map[string]any{"method": r.Method, "path": r.URL.Path}))
	}
}

func methodNotServed(r *http.Request) *Error {
	return Refuse(extapi.ReasonUnsupported, messages.ErrorPluginHostRouteUnknown, "the host API does not serve this method here",
		map[string]any{"method": r.Method, "path": r.URL.Path})
}

func (h *hostConn) push(w http.ResponseWriter, r *http.Request, kind string) {
	owner, refusal := h.waitOwner(r.Context())
	if refusal != nil {
		writeWire(w, refusal)
		return
	}
	if owner.Lifecycle == extapi.LifecycleOnDemand {
		writeWire(w, Refuse(extapi.ReasonPermissionDenied, messages.ErrorPluginStateOnDemand,
			"an on-demand plugin may not push state", map[string]any{"id": owner.ID}))
		return
	}
	if h.state == nil {
		writeWire(w, Refuse(extapi.ReasonNotFound, messages.ErrorPluginHostRouteUnknown, "this server takes no pushed state",
			map[string]any{"method": r.Method, "path": r.URL.Path}))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxPushBytes+1))
	if err != nil {
		writeWire(w, Refuse(extapi.ReasonCancelled, messages.ErrorPluginHostFailed, "reading the push failed",
			map[string]any{"detail": err.Error()}))
		return
	}
	push, err := extapi.DecodeStatePush(raw)
	if err == nil && push.Kind != kind {
		err = &extapi.Error{Code: extapi.ErrorWireInvalid, Message: "the push names another kind than its path",
			Params: map[string]any{"object": "state_push", "field": "kind", "rule": extapi.RuleRelation}}
	}
	if err != nil {
		writeWire(w, wireRefusal(err))
		return
	}
	ack, err := h.state.Push(r.Context(), *owner, r.Method, push)
	if err == nil {
		if verr := ack.Validate(); verr != nil {
			err = verr
			h.log.Error("the state host answered an invalid acknowledgement", "plugin", h.id, "err", verr)
			writeWire(w, Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "invalid acknowledgement",
				map[string]any{"detail": verr.Error()}))
			return
		}
		writeJSON(w, http.StatusOK, ack)
		return
	}
	writeWire(w, wireRefusal(err))
}

// waitOwner waits for the handshake to settle; a push sent right after the
// plugin answered initialize may arrive before the host checked the
// answer.
func (h *hostConn) waitOwner(ctx context.Context) (*Owner, *Error) {
	select {
	case <-h.ready:
	case <-ctx.Done():
		return nil, Refuse(extapi.ReasonCancelled, messages.ErrorPluginHostNotReady, "the call ended before the handshake was accepted", nil)
	}
	if h.refused.Load() {
		return nil, Refuse(extapi.ReasonFailedPrecondition, messages.ErrorPluginHostNotReady, "the handshake was not accepted", nil)
	}
	return h.owner.Load(), nil
}

// wireRefusal maps what a StateHost or the decoder returned to a refusal.
func wireRefusal(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	var x *extapi.Error
	if errors.As(err, &x) {
		reason := extapi.ReasonInvalidArgument
		if x.Code == extapi.ErrorSchemaTooNew {
			reason = extapi.ReasonUnsupported
		}
		w := extapi.WireErrorOf(x, reason)
		w.Message = clipMessage(w.Message)
		return &Error{Status: extapi.StatusForReason(reason), Wire: w}
	}
	return Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "the server failed to handle the call",
		map[string]any{"detail": err.Error()})
}

func writeWire(w http.ResponseWriter, e *Error) {
	wire := e.Wire
	if err := wire.Validate(); err != nil {
		wire = extapi.WireError{Code: messages.ErrorPluginHostFailed, Message: "unencodable refusal",
			Params: extapi.Params{"detail": err.Error()}, Reason: extapi.ReasonInternal}
	}
	status := e.Status
	if status < 400 || status > 599 {
		status = extapi.StatusForReason(wire.Reason)
	}
	writeJSON(w, status, wire)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := extapi.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"code":"PLUGIN_HOST_FAILED","message":"unencodable answer","reason":"internal"}`)
	}
	w.Header().Set("Content-Type", extapi.ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// tokenBucket is a small rate limiter.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

func newTokenBucket(perSecond, burst int, now func() time.Time) *tokenBucket {
	return &tokenBucket{rate: float64(perSecond), burst: float64(burst), tokens: float64(burst), last: now(), now: now}
}

func (b *tokenBucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.now()
	b.tokens = min(b.burst, b.tokens+t.Sub(b.last).Seconds()*b.rate)
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// clipMessage bounds an English fallback to extapi.MaxErrorMessage bytes,
// cut at a rune boundary so the message stays valid UTF-8.
func clipMessage(s string) string {
	if len(s) <= extapi.MaxErrorMessage {
		return s
	}
	n := extapi.MaxErrorMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
