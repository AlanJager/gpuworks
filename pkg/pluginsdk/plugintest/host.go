package plugintest

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Push is one push the fake host received, with its answer.
type Push struct {
	Method   string
	Envelope extapi.StatePush
	// Ack is set when the push was applied; Refusal when it was refused.
	Ack     *extapi.StateAck
	Refusal *extapi.WireError
}

// fakeHost answers the host API the way the server does for the parts a
// plugin can observe: the push envelope and its table decode strictly, the
// path names the kind, generations strictly increase per kind, and an
// applied push is acknowledged with the digest of its canonical body. Its
// refusals carry the server's codes and reasons (a test in the server's
// module compares them); refusals that depend on the server's state, such
// as a rate limit or a permission the administrator did not approve, are
// injected with Harness.RefuseNextPush.
type fakeHost struct {
	mu      sync.Mutex
	held    map[string]extapi.HeldState
	pushes  []Push
	refuse  map[string]extapi.WireError
	changed chan struct{}
	now     func() time.Time
	// handshake is set by Harness.Initialize; nodes and identities are
	// the canned answers of the reads.
	handshake  handshake
	nodes      []extapi.InventoryNode
	identities map[uint64]extapi.NodeIdentity
}

func newFakeHost() *fakeHost {
	return &fakeHost{held: map[string]extapi.HeldState{}, refuse: map[string]extapi.WireError{},
		changed: make(chan struct{}), now: time.Now}
}

// The server's product codes the fake host refuses with. They are the
// server's, not part of pkg/extapi; the server's module tests that its host
// API answers the same inputs with the same codes and reasons.
const (
	codeRouteUnknown = "PLUGIN_HOST_ROUTE_UNKNOWN"
	codeStateStale   = "PLUGIN_STATE_STALE"
)

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == extapi.PathHostHealth:
		if r.Method != http.MethodGet {
			writeRefusal(w, routeUnknown(r, extapi.ReasonUnsupported))
			return
		}
		writeJSON(w, http.StatusOK, extapi.Health{Status: extapi.HealthOK})
	case strings.HasPrefix(r.URL.Path, extapi.PathHostState):
		if r.Method != http.MethodPut && r.Method != http.MethodPatch {
			writeRefusal(w, routeUnknown(r, extapi.ReasonUnsupported))
			return
		}
		h.push(w, r, strings.TrimPrefix(r.URL.Path, extapi.PathHostState))
	case r.URL.Path == extapi.PathHostNodeIdentity:
		if r.Method != http.MethodPost {
			writeRefusal(w, routeUnknown(r, extapi.ReasonUnsupported))
			return
		}
		h.nodeIdentity(w, r)
	case r.URL.Path == extapi.PathHostNodes:
		if r.Method != http.MethodGet {
			writeRefusal(w, routeUnknown(r, extapi.ReasonUnsupported))
			return
		}
		h.inventory(w, r)
	default:
		writeRefusal(w, routeUnknown(r, extapi.ReasonNotFound))
	}
}

func routeUnknown(r *http.Request, reason string) extapi.WireError {
	return extapi.WireError{Code: codeRouteUnknown, Message: "the host API does not serve " + r.Method + " " + r.URL.Path,
		Params: extapi.Params{"method": r.Method, "path": r.URL.Path}, Reason: reason}
}

func (h *fakeHost) push(w http.ResponseWriter, r *http.Request, kind string) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, extapi.MaxPushBytes+1))
	if err != nil {
		writeRefusal(w, *wireOf(err, extapi.ReasonInvalidArgument))
		return
	}
	env, err := extapi.DecodeStatePush(raw)
	if err == nil && env.Kind != kind {
		err = &extapi.Error{Code: extapi.ErrorWireInvalid, Message: "the push names another kind than its path",
			Params: map[string]any{"object": "state_push", "field": "kind", "rule": extapi.RuleRelation}}
	}
	if err != nil {
		h.record(Push{Method: r.Method, Envelope: envOrEmpty(env)}, nil, wireOf(err, extapi.ReasonInvalidArgument), w)
		return
	}
	if r.Method == http.MethodPatch {
		refusal := routeUnknown(r, extapi.ReasonUnsupported)
		refusal.Message = "no pushed table of this server takes a patch"
		h.record(Push{Method: r.Method, Envelope: *env}, nil, &refusal, w)
		return
	}
	if _, err := env.DecodeBody(); err != nil {
		h.record(Push{Method: r.Method, Envelope: *env}, nil, wireOf(err, extapi.ReasonInvalidArgument), w)
		return
	}
	h.mu.Lock()
	if refusal, ok := h.refuse[kind]; ok {
		delete(h.refuse, kind)
		h.mu.Unlock()
		h.record(Push{Method: r.Method, Envelope: *env}, nil, &refusal, w)
		return
	}
	if held, ok := h.held[kind]; ok && env.Generation <= held.Generation {
		h.mu.Unlock()
		h.record(Push{Method: r.Method, Envelope: *env}, nil, &extapi.WireError{Code: codeStateStale,
			Message: fmt.Sprintf("pushed %s at generation %d; the server holds %d", kind, env.Generation, held.Generation),
			Reason:  extapi.ReasonFailedPrecondition,
			Params:  extapi.Params{"kind": kind, "generation": int64(env.Generation), "held": int64(held.Generation)}}, w)
		return
	}
	h.mu.Unlock()
	canonical, err := extapi.Canonical(env.Body)
	if err != nil {
		h.record(Push{Method: r.Method, Envelope: *env}, nil, wireOf(err, extapi.ReasonInvalidArgument), w)
		return
	}
	at := extapi.NewTime(h.now())
	ack := &extapi.StateAck{Kind: kind, Generation: env.Generation, Digest: extapi.DigestOf(canonical), AppliedAt: &at}
	h.mu.Lock()
	h.held[kind] = extapi.HeldState{Kind: kind, Schema: env.Schema, Generation: env.Generation, Digest: ack.Digest}
	h.mu.Unlock()
	h.record(Push{Method: r.Method, Envelope: *env}, ack, nil, w)
}

func envOrEmpty(env *extapi.StatePush) extapi.StatePush {
	if env == nil {
		return extapi.StatePush{}
	}
	return *env
}

func (h *fakeHost) record(p Push, ack *extapi.StateAck, refusal *extapi.WireError, w http.ResponseWriter) {
	p.Ack, p.Refusal = ack, refusal
	h.mu.Lock()
	h.pushes = append(h.pushes, p)
	close(h.changed)
	h.changed = make(chan struct{})
	h.mu.Unlock()
	if refusal != nil {
		writeRefusal(w, *refusal)
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

func wireOf(err error, reason string) *extapi.WireError {
	e, ok := err.(*extapi.Error)
	if ok && e.Code == extapi.ErrorSchemaTooNew {
		reason = extapi.ReasonUnsupported
	}
	if !ok {
		e = &extapi.Error{Code: extapi.ErrorWireMalformed, Message: err.Error(),
			Params: map[string]any{"object": "state_push", "field": "_root", "rule": extapi.RuleSyntax}}
	}
	w := extapi.WireErrorOf(e, reason)
	return &w
}

func writeRefusal(w http.ResponseWriter, e extapi.WireError) {
	writeJSON(w, extapi.StatusForReason(e.Reason), e)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := extapi.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", extapi.ContentTypeJSON)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
