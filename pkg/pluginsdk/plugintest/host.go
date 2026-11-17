package plugintest

import (
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
// applied push is acknowledged with the digest of its canonical body.
type fakeHost struct {
	mu      sync.Mutex
	held    map[string]extapi.HeldState
	pushes  []Push
	refuse  map[string]extapi.WireError
	changed chan struct{}
	now     func() time.Time
}

func newFakeHost() *fakeHost {
	return &fakeHost{held: map[string]extapi.HeldState{}, refuse: map[string]extapi.WireError{},
		changed: make(chan struct{}), now: time.Now}
}

func (h *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == extapi.PathHostHealth && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, extapi.Health{Status: extapi.HealthOK})
	case strings.HasPrefix(r.URL.Path, extapi.PathHostState) && (r.Method == http.MethodPut || r.Method == http.MethodPatch):
		h.push(w, r, strings.TrimPrefix(r.URL.Path, extapi.PathHostState))
	default:
		writeRefusal(w, extapi.WireError{Code: extapi.ErrorWireInvalid, Message: "no such host endpoint: " + r.Method + " " + r.URL.Path,
			Params: extapi.Params{"object": "request", "field": "path", "rule": extapi.RuleEnum}, Reason: extapi.ReasonNotFound})
	}
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
		h.record(Push{Method: r.Method, Envelope: *env}, nil, &extapi.WireError{Code: extapi.ErrorWireInvalid,
			Message: "no kind of protocol major 1 has a patch form", Reason: extapi.ReasonUnsupported,
			Params: extapi.Params{"object": "state_push", "field": "base", "rule": extapi.RuleRelation}}, w)
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
		h.record(Push{Method: r.Method, Envelope: *env}, nil, &extapi.WireError{Code: extapi.ErrorWireInvalid,
			Message: "the generation is not newer than the one held", Reason: extapi.ReasonConflict,
			Params: extapi.Params{"object": "state_push", "field": "generation", "rule": extapi.RuleRelation}}, w)
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
