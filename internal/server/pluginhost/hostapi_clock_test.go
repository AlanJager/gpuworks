package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// fakeServices records what the host API hands its services and answers as
// told.
type fakeServices struct {
	mu        sync.Mutex
	corrected []string
	readies   []uint64
	ownedFor  []string
	keysFor   []string
	refuse    error
	keyLength int // when set, the deriver answers a key of this length
}

func (f *fakeServices) Clocks() (wall, ext, hwm time.Time) {
	return time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
}

func (f *fakeServices) Correct(_ context.Context, o Owner, to time.Time, reason string) (time.Time, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return time.Time{}, time.Time{}, f.refuse
	}
	f.corrected = append(f.corrected, o.ID+" "+to.Format(time.RFC3339)+" "+reason)
	return time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), to, nil
}

func (f *fakeServices) Ready(o Owner) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readies = append(f.readies, o.Generation)
}

func (f *fakeServices) OwnRows(o Owner, kind string) (extapi.PushedState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ownedFor = append(f.ownedFor, o.ID+"/"+kind)
	if f.refuse != nil {
		return extapi.PushedState{}, f.refuse
	}
	return extapi.PushedState{Kind: kind}, nil
}

func (f *fakeServices) DeriveKey(o Owner, label string, length int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keysFor = append(f.keysFor, o.ID+"/"+label)
	if f.keyLength != 0 {
		length = f.keyLength
	}
	return bytes.Repeat([]byte{7}, length), nil
}

func servicesConn(t *testing.T, f *fakeServices, o Owner) *hostConn {
	t.Helper()
	h := testHostConn(t, &fakeState{}, o.Lifecycle)
	cfg := Config{Core: testCore, ExtClock: f, Ready: f, OwnState: f, Keys: f}
	svc, err := newHostServices(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	h.accept(o)
	return h
}

// authority is an owner granted the clock role, the key permission and
// every capability of these calls.
func authority() Owner {
	o := owner()
	o.ID = "acme_quota"
	o.Generation = 3
	o.Grant = extapi.PluginGrant{ID: "acme_quota", Version: "1.0.0", PermissionsGranted: []string{"crypto:derive_key"},
		RolesGranted: []string{"availability_authority", "clock_authority"}}
	o.Use = map[string]int{extapi.CapHostClock: 1, extapi.CapHostStateRead: 1, extapi.CapHostDeriveKey: 1}
	return o
}

// raw makes one call and returns the status and body; a refusal must be a
// wire error.
func raw(t *testing.T, h *hostConn, method, path, body string) (int, []byte, *extapi.WireError) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	if rec.Code/100 == 2 {
		return rec.Code, rec.Body.Bytes(), nil
	}
	w, err := extapi.DecodeWireError(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("%s %s: %d %s: %v", method, path, rec.Code, rec.Body.Bytes(), err)
	}
	return rec.Code, nil, w
}

func wantRefusal(t *testing.T, what string, status int, w *extapi.WireError, wantStatus int, code string, params map[string]any) {
	t.Helper()
	if w == nil || status != wantStatus || w.Code != code {
		t.Fatalf("%s: %d %+v, want %d %s", what, status, w, wantStatus, code)
	}
	for k, v := range params {
		if fmt.Sprint(w.Params[k]) != fmt.Sprint(v) {
			t.Fatalf("%s: params %v, want %s=%v", what, w.Params, k, v)
		}
	}
}

const correctBody = `{"reason":"acme correction code 7F3A-22C1","to":"2027-01-15T00:00:00Z"}`

func TestClockCorrectionNeedsTheRoleAndTheCapability(t *testing.T) {
	f := &fakeServices{}
	noRole := authority()
	noRole.Grant.RolesGranted = []string{"availability_authority"}
	st, _, w := raw(t, servicesConn(t, f, noRole), http.MethodPost, extapi.PathHostClockCorrect, correctBody)
	wantRefusal(t, "without the role", st, w, 403, messages.ErrorPluginRoleRequired, map[string]any{"id": "acme_quota", "role": "clock_authority"})
	if w.Reason != extapi.ReasonPermissionDenied {
		t.Fatalf("reason %s", w.Reason)
	}
	noCap := authority()
	noCap.Use = map[string]int{}
	st, _, w = raw(t, servicesConn(t, f, noCap), http.MethodPost, extapi.PathHostClockCorrect, correctBody)
	wantRefusal(t, "without host.clock", st, w, 412, messages.ErrorPluginCapabilityNotOffered, map[string]any{"capability": "host.clock"})
	if len(f.corrected) != 0 {
		t.Fatalf("a refused correction reached the clock: %v", f.corrected)
	}

	h := servicesConn(t, f, authority())
	st, body, w := raw(t, h, http.MethodPost, extapi.PathHostClockCorrect, correctBody)
	if w != nil || st != 200 || string(body) != `{"from":"2027-03-01T00:00:00Z","to":"2027-01-15T00:00:00Z"}` {
		t.Fatalf("correction = %d %s %+v", st, body, w)
	}
	if len(f.corrected) != 1 || f.corrected[0] != "acme_quota 2027-01-15T00:00:00Z acme correction code 7F3A-22C1" {
		t.Fatalf("corrected = %v", f.corrected)
	}

	st, _, w = raw(t, h, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x"}`)
	wantRefusal(t, "no time", st, w, 400, messages.ErrorPluginHostArgumentInvalid, map[string]any{"id": "acme_quota", "field": "to"})
	st, _, w = raw(t, h, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"x","to":"2027-01-15T00:00:00Z","by":"root"}`)
	wantRefusal(t, "unknown field", st, w, 400, extapi.ErrorWireUnknownField, nil)
	st, _, w = raw(t, h, http.MethodPost, extapi.PathHostClockCorrect, `{"reason":"`+strings.Repeat("a", extapi.MaxHostCallBytes)+`","to":"2027-01-15T00:00:00Z"}`)
	wantRefusal(t, "oversized", st, w, 413, messages.ErrorRequestTooLarge, map[string]any{"max": int64(extapi.MaxHostCallBytes)})
	st, _, w = raw(t, h, http.MethodGet, extapi.PathHostClockCorrect, "")
	wantRefusal(t, "GET", st, w, 501, messages.ErrorPluginHostRouteUnknown, nil)

	// The clock's own refusal (a correction beyond the bound) is answered
	// as it is.
	f.refuse = Refuse(extapi.ReasonInvalidArgument, messages.ErrorPluginClockCorrectionTooLarge, "too far", map[string]any{"max_days": 366})
	st, _, w = raw(t, h, http.MethodPost, extapi.PathHostClockCorrect, correctBody)
	wantRefusal(t, "too far", st, w, 400, messages.ErrorPluginClockCorrectionTooLarge, map[string]any{"max_days": int64(366)})
}

func TestMetaAnswersTheServerAndItsClocks(t *testing.T) {
	h := servicesConn(t, &fakeServices{}, authority())
	st, body, w := raw(t, h, http.MethodGet, extapi.PathHostMeta, "")
	if w != nil || st != 200 {
		t.Fatalf("meta = %d %+v", st, w)
	}
	m, err := extapi.DecodeHostMeta(body)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "v26.12.0" || m.BootID != h.svc.bootID || m.Wall.Format(time.RFC3339) != "2027-01-15T00:00:00Z" ||
		m.ExtClock.Format(time.RFC3339) != "2027-03-01T00:00:00Z" || m.HWM == nil {
		t.Fatalf("meta = %s", body)
	}
	other := servicesConn(t, &fakeServices{}, authority())
	if other.svc.bootID == h.svc.bootID {
		t.Fatal("two hosts share a boot id")
	}
	noCap := authority()
	delete(noCap.Use, extapi.CapHostClock)
	st, _, w = raw(t, servicesConn(t, &fakeServices{}, noCap), http.MethodGet, extapi.PathHostMeta, "")
	wantRefusal(t, "without host.clock", st, w, 412, messages.ErrorPluginCapabilityNotOffered, nil)
	bare := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	bare.accept(authority())
	st, _, w = raw(t, bare, http.MethodGet, extapi.PathHostMeta, "")
	wantRefusal(t, "without a clock", st, w, 404, messages.ErrorPluginHostRouteUnknown, nil)
}

// Ready is answered 204 and reaches the ready host with the connection's
// process generation, except from an on-demand plugin; it waits for the
// handshake like any call.
func TestReadyReachesTheReadyHostWithTheProcessGeneration(t *testing.T) {
	f := &fakeServices{}
	h := servicesConn(t, f, authority())
	for range 2 {
		if st, _, w := raw(t, h, http.MethodPost, extapi.PathHostReady, ""); st != 204 || w != nil {
			t.Fatalf("ready = %d %+v", st, w)
		}
	}
	if len(f.readies) != 2 || f.readies[0] != 3 {
		t.Fatalf("readies = %v", f.readies)
	}
	od := authority()
	od.Lifecycle = extapi.LifecycleOnDemand
	if st, _, _ := raw(t, servicesConn(t, f, od), http.MethodPost, extapi.PathHostReady, ""); st != 204 || len(f.readies) != 2 {
		t.Fatalf("on-demand ready = %d, readies %v", st, f.readies)
	}
	st, _, w := raw(t, h, http.MethodGet, extapi.PathHostReady, "")
	wantRefusal(t, "GET ready", st, w, 501, messages.ErrorPluginHostRouteUnknown, nil)
	rejected := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	rejected.reject()
	st, _, w = raw(t, rejected, http.MethodPost, extapi.PathHostReady, "")
	wantRefusal(t, "ready on a refused connection", st, w, 412, messages.ErrorPluginHostNotReady, nil)
	bare := testHostConn(t, &fakeState{}, extapi.LifecycleResident)
	bare.accept(authority())
	if st, _, _ := raw(t, bare, http.MethodPost, extapi.PathHostReady, ""); st != 204 {
		t.Fatalf("ready without a ready host = %d", st)
	}
}

// The state read names no owner: it is the connection's, so a plugin reads
// only its own tables.
func TestOwnStateReadsTheConnectionsOwnTables(t *testing.T) {
	f := &fakeServices{}
	h := servicesConn(t, f, authority())
	st, body, w := raw(t, h, http.MethodGet, extapi.PathHostState+"banners", "")
	if w != nil || st != 200 || string(body) != `{"kind":"banners"}` {
		t.Fatalf("own state = %d %s %+v", st, body, w)
	}
	if len(f.ownedFor) != 1 || f.ownedFor[0] != "acme_quota/banners" {
		t.Fatalf("read for %v", f.ownedFor)
	}
	st, _, w = raw(t, h, http.MethodGet, extapi.PathHostState+"Banners", "")
	wantRefusal(t, "bad kind", st, w, 400, messages.ErrorPluginHostArgumentInvalid, map[string]any{"field": "kind"})
	noCap := authority()
	noCap.Use = map[string]int{}
	st, _, w = raw(t, servicesConn(t, f, noCap), http.MethodGet, extapi.PathHostState+"banners", "")
	wantRefusal(t, "without host.state_read", st, w, 412, messages.ErrorPluginCapabilityNotOffered, map[string]any{"capability": "host.state_read"})
	f.refuse = Refuse(extapi.ReasonInternal, messages.ErrorPluginHostFailed, "broken", map[string]any{"detail": "x"})
	st, _, w = raw(t, h, http.MethodGet, extapi.PathHostState+"banners", "")
	wantRefusal(t, "a failing read", st, w, 500, messages.ErrorPluginHostFailed, nil)
	if len(f.ownedFor) != 2 {
		t.Fatalf("refused reads reached the state: %v", f.ownedFor)
	}
}

func TestDeriveKeyNeedsThePermissionAndBoundedArguments(t *testing.T) {
	f := &fakeServices{}
	noPerm := authority()
	noPerm.Grant.PermissionsGranted = nil
	st, _, w := raw(t, servicesConn(t, f, noPerm), http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`)
	wantRefusal(t, "without the permission", st, w, 403, messages.ErrorPluginHostNotPermitted, map[string]any{"id": "acme_quota", "permission": "crypto:derive_key"})
	noCap := authority()
	noCap.Use = map[string]int{}
	st, _, w = raw(t, servicesConn(t, f, noCap), http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":32}`)
	wantRefusal(t, "without host.derive_key", st, w, 412, messages.ErrorPluginCapabilityNotOffered, map[string]any{"capability": "host.derive_key"})

	h := servicesConn(t, f, authority())
	for _, tc := range []struct{ body, field string }{
		{`{"label":"USB","length":32}`, "label"},
		{`{"label":"` + strings.Repeat("a", 65) + `","length":32}`, "label"},
		{`{"label":"usb","length":15}`, "length"},
		{`{"label":"usb","length":65}`, "length"},
	} {
		st, _, w := raw(t, h, http.MethodPost, extapi.PathHostDeriveKey, tc.body)
		wantRefusal(t, tc.body, st, w, 400, messages.ErrorPluginHostArgumentInvalid, map[string]any{"field": tc.field})
	}
	if len(f.keysFor) != 0 {
		t.Fatalf("refused requests reached the deriver: %v", f.keysFor)
	}
	st, body, w := raw(t, h, http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":16}`)
	if w != nil || st != 200 {
		t.Fatalf("derive = %d %+v", st, w)
	}
	k, err := extapi.DecodeDerivedKey(body)
	if err != nil || len(k.Key) != 16 || f.keysFor[0] != "acme_quota/usb.challenge" {
		t.Fatalf("key = %s, %v, %v", body, err, f.keysFor)
	}
	f.keyLength = 20
	st, _, w = raw(t, h, http.MethodPost, extapi.PathHostDeriveKey, `{"label":"usb.challenge","length":16}`)
	wantRefusal(t, "a key of another length", st, w, 500, messages.ErrorPluginHostFailed, nil)
}
