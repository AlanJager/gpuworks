package pluginhost

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func testHostConn(t *testing.T, state StateHost, lifecycle string) *hostConn {
	t.Helper()
	spec := testSpec(t, "acme_backup")
	spec.Lifecycle = lifecycle
	return newHostConn(&spec, state, nil, slog.New(slog.DiscardHandler))
}

func owner() Owner {
	return Owner{ID: "acme_backup", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Use: map[string]int{"table.banners": 1}}
}

type answer struct {
	status int
	wire   *extapi.WireError
	ack    *extapi.StateAck
	health *extapi.Health
}

func call(t *testing.T, h *hostConn, ctx context.Context, method, path, body string) answer {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	a := answer{status: rec.Code}
	raw := rec.Body.Bytes()
	if rec.Header().Get("Content-Type") != extapi.ContentTypeJSON {
		t.Fatalf("%s %s: content type %q", method, path, rec.Header().Get("Content-Type"))
	}
	switch {
	case rec.Code/100 != 2:
		w, err := extapi.DecodeWireError(raw)
		if err != nil {
			t.Fatalf("%s %s: %d with %s: %v", method, path, rec.Code, raw, err)
		}
		a.wire = w
	case strings.HasPrefix(path, extapi.PathHostState):
		ack, err := extapi.DecodeStateAck(raw)
		if err != nil {
			t.Fatalf("ack %s: %v", raw, err)
		}
		a.ack = ack
	default:
		hl, err := extapi.DecodeHealth(raw)
		if err != nil {
			t.Fatalf("health %s: %v", raw, err)
		}
		a.health = hl
	}
	return a
}

const pushBanners = `{"body":{},"generation":7,"kind":"banners","schema":1}`

// The host API answers every call with a gwx/1 body: the push the state
// host accepted, or a coded refusal under the status of its reason.
func TestHostAPIAnswers(t *testing.T) {
	ctx := context.Background()
	state := &fakeState{}
	h := testHostConn(t, state, extapi.LifecycleResident)
	h.accept(owner())

	if a := call(t, h, ctx, http.MethodGet, extapi.PathHostHealth, ""); a.status != 200 || a.health.Status != extapi.HealthOK {
		t.Fatalf("health = %+v", a)
	}
	a := call(t, h, ctx, http.MethodPut, extapi.PathHostState+"banners", pushBanners)
	if a.status != 200 || a.ack.Generation != 7 || a.ack.AppliedAt == nil {
		t.Fatalf("push = %+v", a)
	}
	if got := state.recorded(); len(got) != 1 || got[0].owner.ID != "acme_backup" || got[0].push.Generation != 7 {
		t.Fatalf("recorded = %+v", got)
	}
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"kind mismatch", http.MethodPut, extapi.PathHostState + "node_holds", pushBanners, 400, extapi.ErrorWireInvalid},
		{"malformed", http.MethodPut, extapi.PathHostState + "banners", `{"kind":`, 400, extapi.ErrorWireMalformed},
		{"unknown field", http.MethodPut, extapi.PathHostState + "banners", `{"body":{},"generation":1,"kind":"banners","schema":1,"owner":"x"}`, 400, extapi.ErrorWireUnknownField},
		{"unknown route", http.MethodGet, "/host/v1/directory/users", "", 404, messages.ErrorPluginHostRouteUnknown},
		{"wrong method", http.MethodDelete, extapi.PathHostState + "banners", "", 501, messages.ErrorPluginHostRouteUnknown},
		{"wrong health method", http.MethodPost, extapi.PathHostHealth, "", 501, messages.ErrorPluginHostRouteUnknown},
	}
	for _, tc := range cases {
		a := call(t, h, ctx, tc.method, tc.path, tc.body)
		if a.status != tc.status || a.wire.Code != tc.code {
			t.Errorf("%s: %d %+v, want %d %s", tc.name, a.status, a.wire, tc.status, tc.code)
		}
	}
	if n := len(state.recorded()); n != 1 {
		t.Fatalf("refused pushes reached the state host (%d)", n)
	}
	big := `{"body":{"x":"` + strings.Repeat("a", extapi.MaxPushBytes) + `"},"generation":1,"kind":"banners","schema":1}`
	if a := call(t, h, ctx, http.MethodPut, extapi.PathHostState+"banners", big); a.wire.Code != extapi.ErrorStateTooLarge {
		t.Fatalf("oversize push = %+v", a)
	}
}

// What the state host returns decides the answer: its refusal as is, a
// wire error as invalid_argument, an invalid acknowledgement or any other
// failure as PLUGIN_HOST_FAILED.
func TestHostAPIStateHostOutcomes(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		refuse error
		ack    func(*extapi.StatePush) *extapi.StateAck
		status int
		code   string
	}{
		"refusal": {refuse: Refuse(extapi.ReasonPermissionDenied, "PLUGIN_STATE_NOT_OWNER", "not yours", map[string]any{"kind": "banners"}),
			status: 403, code: "PLUGIN_STATE_NOT_OWNER"},
		"wire error": {refuse: &extapi.Error{Code: extapi.ErrorStateRejected, Message: "bad row",
			Params: map[string]any{"kind": "banners", "field": "banners.0.id", "rule": "grammar", "row": "0"}}, status: 400, code: extapi.ErrorStateRejected},
		"too new": {refuse: &extapi.Error{Code: extapi.ErrorSchemaTooNew, Message: "too new",
			Params: map[string]any{"object": "table.banners", "version": 2, "known": 1}}, status: 501, code: extapi.ErrorSchemaTooNew},
		"failure":     {refuse: errors.New("database is locked"), status: 500, code: messages.ErrorPluginHostFailed},
		"invalid ack": {ack: func(p *extapi.StatePush) *extapi.StateAck { return &extapi.StateAck{Kind: p.Kind} }, status: 500, code: messages.ErrorPluginHostFailed},
	} {
		t.Run(name, func(t *testing.T) {
			h := testHostConn(t, &fakeState{refuse: tc.refuse, ack: tc.ack}, extapi.LifecycleResident)
			h.accept(owner())
			a := call(t, h, ctx, http.MethodPut, extapi.PathHostState+"banners", pushBanners)
			if a.status != tc.status || a.wire.Code != tc.code {
				t.Fatalf("%d %+v", a.status, a.wire)
			}
		})
	}
}

// A push waits for the handshake to settle: it proceeds once accepted, is
// refused once rejected, and gives up with its caller.
func TestHostAPIWaitsForTheHandshake(t *testing.T) {
	state := &fakeState{}
	h := testHostConn(t, state, extapi.LifecycleResident)
	got := make(chan answer, 1)
	go func() {
		got <- call(t, h, context.Background(), http.MethodPut, extapi.PathHostState+"banners", pushBanners)
	}()
	select {
	case a := <-got:
		t.Fatalf("a push before the handshake was answered: %+v", a)
	case <-time.After(50 * time.Millisecond):
	}
	h.accept(owner())
	if a := <-got; a.status != 200 {
		t.Fatalf("push after acceptance = %+v", a)
	}

	rejected := testHostConn(t, state, extapi.LifecycleResident)
	rejected.reject()
	rejected.accept(owner()) // settles once: stays rejected
	if a := call(t, rejected, context.Background(), http.MethodPut, extapi.PathHostState+"banners", pushBanners); a.status != 412 || a.wire.Code != messages.ErrorPluginHostNotReady {
		t.Fatalf("push on a rejected connection = %+v", a)
	}

	pending := testHostConn(t, state, extapi.LifecycleResident)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if a := call(t, pending, ctx, http.MethodPut, extapi.PathHostState+"banners", pushBanners); a.wire.Code != messages.ErrorPluginHostNotReady || a.wire.Reason != extapi.ReasonCancelled {
		t.Fatalf("push that gave up = %+v", a)
	}
}

func TestHostAPIRefusesWithoutAStateHostAndForOnDemand(t *testing.T) {
	ctx := context.Background()
	none := testHostConn(t, nil, extapi.LifecycleResident)
	none.accept(owner())
	if a := call(t, none, ctx, http.MethodPut, extapi.PathHostState+"banners", pushBanners); a.status != 404 {
		t.Fatalf("push without a state host = %+v", a)
	}
	od := testHostConn(t, &fakeState{}, extapi.LifecycleOnDemand)
	o := owner()
	o.Lifecycle = extapi.LifecycleOnDemand
	od.accept(o)
	if a := call(t, od, ctx, http.MethodPut, extapi.PathHostState+"banners", pushBanners); a.status != 403 || a.wire.Code != messages.ErrorPluginStateOnDemand {
		t.Fatalf("on-demand push = %+v", a)
	}
}

// The host API allows a burst, then refuses with a retry hint until the
// bucket refills.
func TestHostAPIRateLimit(t *testing.T) {
	h := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	now := time.Unix(1000, 0)
	h.limiter = newTokenBucket(hostRatePerSecond, hostRateBurst, func() time.Time { return now })
	ctx := context.Background()
	for i := 0; i < hostRateBurst; i++ {
		if a := call(t, h, ctx, http.MethodGet, extapi.PathHostHealth, ""); a.status != 200 {
			t.Fatalf("call %d = %+v", i, a)
		}
	}
	a := call(t, h, ctx, http.MethodGet, extapi.PathHostHealth, "")
	if a.status != 429 || a.wire.Code != messages.ErrorPluginHostRateLimited || a.wire.RetryAfterMS != 50 {
		t.Fatalf("over the burst = %+v", a)
	}
	now = now.Add(100 * time.Millisecond)
	for i := 0; i < 2; i++ {
		if a := call(t, h, ctx, http.MethodGet, extapi.PathHostHealth, ""); a.status != 200 {
			t.Fatalf("after refill, call %d = %+v", i, a)
		}
	}
	if a := call(t, h, ctx, http.MethodGet, extapi.PathHostHealth, ""); a.status != 429 {
		t.Fatalf("refill granted more than the rate: %+v", a)
	}
}
