package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fakeDynamic is a plugin host for the API: a route table it matches as
// the real one does (literal segments before {name} parameters) and a
// plugin served by a real HTTP server, so streams, aborts and headers
// behave as they do on a plugin's connection.
type fakeDynamic struct {
	t      *testing.T
	mu     sync.Mutex
	routes []DynamicRoute
	plugin http.Handler
	srv    *httptest.Server
	// refuse, when set, refuses the call as the plugin host would.
	refuse func(*DynamicRoute) error
	calls  atomic.Int32
	last   atomic.Pointer[forwarded]
	idle   time.Duration
}

// forwarded is what the plugin host was handed for one call.
type forwarded struct {
	route  DynamicRoute
	path   string
	header http.Header
	caller DynamicCaller
}

func newFakeDynamic(t *testing.T, routes ...DynamicRoute) *fakeDynamic {
	f := &fakeDynamic{t: t, routes: routes}
	f.plugin = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"path": r.URL.RequestURI(), "method": r.Method})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		h := f.plugin
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDynamic) serve(h http.HandlerFunc) {
	f.mu.Lock()
	f.plugin = h
	f.mu.Unlock()
}

func (f *fakeDynamic) ext() *DynamicExtension {
	return &DynamicExtension{Match: f.match, Routes: f.list, Forward: f.forward, DecodeError: fakeDecodeError, StreamIdle: f.idle}
}

func (f *fakeDynamic) list() []DynamicRoute {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.routes)
}

func (f *fakeDynamic) match(method, path string) (*DynamicRoute, DynamicMatch) {
	id, rest, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok {
		return nil, DynamicMiss
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *DynamicRoute
	pathHit := false
	for i := range f.routes {
		r := &f.routes[i]
		if r.Plugin != id || !patternMatches(r.Pattern, "/"+rest) {
			continue
		}
		pathHit = true
		if r.Method == method && (found == nil || strings.Count(found.Pattern, "{") > strings.Count(r.Pattern, "{")) {
			found = r
		}
	}
	switch {
	case found != nil:
		return found, DynamicFound
	case pathHit:
		return nil, DynamicMethodNotAllowed
	}
	return nil, DynamicMiss
}

func patternMatches(pattern, path string) bool {
	p, s := strings.Split(pattern, "/"), strings.Split(path, "/")
	if len(p) != len(s) {
		return false
	}
	for i := range p {
		if p[i] != s[i] && !(strings.HasPrefix(p[i], "{") && s[i] != "") {
			return false
		}
	}
	return true
}

func (f *fakeDynamic) forward(ctx context.Context, route *DynamicRoute, req *http.Request, c DynamicCaller) (*http.Response, error) {
	f.calls.Add(1)
	f.last.Store(&forwarded{route: *route, path: req.URL.RequestURI(), header: req.Header.Clone(), caller: c})
	if f.refuse != nil {
		if err := f.refuse(route); err != nil {
			return nil, err
		}
	}
	req.URL.Scheme, req.URL.Host, req.Host = "http", strings.TrimPrefix(f.srv.URL, "http://"), ""
	return (&http.Transport{}).RoundTrip(req)
}

// fakeDecodeError accepts what a gwx/1 error body must have: a code and a
// reason.
func fakeDecodeError(raw []byte) (*messages.ProductError, error) {
	var e struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Params  map[string]any `json:"params"`
		Reason  string         `json:"reason"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || e.Code == "" || e.Reason == "" {
		return nil, errors.New("not a gwx/1 error")
	}
	return messages.NewProductError(e.Code, e.Message, e.Params), nil
}

func dynRoute(method, pattern string, access DynamicAccess, edit ...func(*DynamicRoute)) DynamicRoute {
	r := DynamicRoute{Plugin: "acme_fake", Version: "1.0.0", Method: method, Pattern: pattern, Action: "acme_fake.x", Access: access,
		MaxBody: 1 << 20, Timeout: 5 * time.Second, Codes: []string{"ACME_BACKUP_RUNNING"}}
	for _, e := range edit {
		e(&r)
	}
	return r
}

// dynamicRig is a router with the fake plugin host mounted, its feature
// gate's answers, and callers of each kind.
type dynamicRig struct {
	*tokenRig
	dyn           *fakeDynamic
	src           *switchSource
	member        string
	read, write   string
	sessionCookie string
	signer        *auth.TokenSigner
}

func newDynamicRig(t *testing.T, routes []DynamicRoute, with ...func(*Deps)) *dynamicRig {
	t.Helper()
	d := &dynamicRig{dyn: newFakeDynamic(t, routes...), src: &switchSource{}}
	d.src.set(map[featuregate.Key]featuregate.Availability{"acme.backup": {Access: featuregate.Full}})
	gate := mustGate(t, d.src, featuregate.Feature{Key: "acme.backup"}, featuregate.Feature{Key: "acme.hidden", Presentation: featuregate.Conceal})
	d.tokenRig = newTokenRig(t, append([]func(*Deps){func(deps *Deps) {
		deps.Extensions = ExtensionDeps{Features: gate, Dynamic: d.dyn.ext()}
		d.signer = deps.AuthSigner
	}}, with...)...)
	var err error
	if d.member, err = managementSessionTestToken(t, d.st, d.signer, auth.Claims{Username: "member", Role: "user"}); err != nil {
		t.Fatal(err)
	}
	d.read, d.write = d.mint("read"), d.mint("write")
	admin, _ := d.st.VerifyUser("admin", "gpuworks-test-password-2026")
	raw, _, err := d.st.CreateManagementSession(admin, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d.sessionCookie = raw
	return d
}

func codeOfBody(rec *httptest.ResponseRecorder) string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code
}

// Who may call which route: the core's authentication and roles answer
// before the plugin is asked, and what the plugin hears names the caller.
func TestDynamicRoutesAuthenticateByAccess(t *testing.T) {
	rig := newDynamicRig(t, []DynamicRoute{
		dynRoute("POST", "/admin", DynamicAdmin),
		dynRoute("POST", "/session", DynamicAdminSession),
		dynRoute("GET", "/items", DynamicMember),
		dynRoute("POST", "/items", DynamicMember),
		dynRoute("GET", "/hook", DynamicPublic, func(r *DynamicRoute) { r.RatePerMinute, r.RateBurst = 1, 2 }),
	})
	for _, tc := range []struct {
		name, bearer, method, path string
		status                     int
		code, kind                 string
	}{
		{"anonymous", "", "POST", "/api/v1/x/acme_fake/admin", 401, "AUTHENTICATION_REQUIRED", ""},
		{"member on admin", rig.member, "POST", "/api/v1/x/acme_fake/admin", 403, "ADMINISTRATOR_REQUIRED", ""},
		{"write token on admin", rig.write, "POST", "/api/v1/x/acme_fake/admin", 200, "", "token"},
		{"read token on admin write", rig.read, "POST", "/api/v1/x/acme_fake/admin", 403, "PLATFORM_TOKEN_READ_ONLY", ""},
		{"platform token on admin_session", rig.write, "POST", "/api/v1/x/acme_fake/session", 403, "PLATFORM_TOKEN_NOT_ALLOWED", ""},
		{"admin session on admin_session", rig.session, "POST", "/api/v1/x/acme_fake/session", 200, "", "session"},
		{"member reads", rig.member, "GET", "/api/v1/x/acme_fake/items", 200, "", "session"},
		{"read token writes", rig.read, "POST", "/api/v1/x/acme_fake/items", 403, "PLATFORM_TOKEN_READ_ONLY", ""},
		{"read token reads", rig.read, "GET", "/api/v1/x/acme_fake/items", 200, "", "token"},
		{"public, anonymous", "", "GET", "/api/v1/x/acme_fake/hook", 200, "", "public"},
	} {
		before := rig.dyn.calls.Load()
		rec := rig.do(tc.bearer, tc.method, tc.path, "")
		if rec.Code != tc.status || (tc.code != "" && codeOfBody(rec) != tc.code) {
			t.Fatalf("%s: %d %s, want %d %s", tc.name, rec.Code, rec.Body, tc.status, tc.code)
		}
		called := rig.dyn.calls.Load() > before
		if called != (tc.status == 200) {
			t.Fatalf("%s: plugin called = %t", tc.name, called)
		}
		if called && rig.dyn.last.Load().caller.Kind != tc.kind {
			t.Fatalf("%s: caller %+v", tc.name, rig.dyn.last.Load().caller)
		}
	}
	if c := rig.dyn.last.Load().caller; c.UserID != 0 || c.Username != "" {
		t.Fatalf("a public caller carries a user: %+v", c)
	}
	// The burst of two is spent (one above): the next is refused, and no
	// call reaches the plugin.
	before := rig.dyn.calls.Load()
	rig.do("", "GET", "/api/v1/x/acme_fake/hook", "")
	rec := rig.do("", "GET", "/api/v1/x/acme_fake/hook", "")
	if rec.Code != http.StatusTooManyRequests || strings.TrimSpace(rec.Body.String()) != `{"code":"TOO_MANY_REQUESTS","error":"too many requests to this route; try again later"}` ||
		rec.Header().Get("Retry-After") == "" || rig.dyn.calls.Load() != before+1 {
		t.Fatalf("over the rate = %d %s %v", rec.Code, rec.Body, rec.Header())
	}
}

// Behind a trusted proxy a public route counts the forwarded client, so
// two clients behind one proxy have a bucket each.
func TestDynamicPublicRateFollowsTheForwardedClient(t *testing.T) {
	dyn := newFakeDynamic(t, dynRoute("GET", "/hook", DynamicPublic, func(r *DynamicRoute) { r.RatePerMinute, r.RateBurst = 1, 1 }))
	h := Router(Deps{Extensions: ExtensionDeps{Dynamic: dyn.ext()}}, auth.SessionPolicy{Proxies: listedProxies("192.0.2.1")})
	get := func(client string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/x/acme_fake/hook", nil)
		req.Header.Set("X-Forwarded-For", client)
		return serve(h, req).Code
	}
	if a, b, a2 := get("203.0.113.7"), get("203.0.113.8"), get("203.0.113.7"); a != 200 || b != 200 || a2 != http.StatusTooManyRequests {
		t.Fatalf("forwarded clients = %d %d %d", a, b, a2)
	}
	// Addresses in one IPv6 /64 share a bucket.
	if a, b := get("2001:db8::1"), get("2001:db8::2"); a != 200 || b != http.StatusTooManyRequests {
		t.Fatalf("one /64 = %d %d", a, b)
	}
}

// The bucket map stays bounded however many addresses call.
func TestPublicLimiterIsBounded(t *testing.T) {
	l := newPublicLimiter(100)
	r := dynRoute("GET", "/hook", DynamicPublic)
	now := time.Now()
	for i := range 1000 {
		l.take(&r, fmt.Sprintf("198.51.%d.%d", i/256, i%256), now)
	}
	if l.size() != 100 {
		t.Fatalf("limiter holds %d buckets", l.size())
	}
	// The scale cases read the mounted limiter's size through the router.
	rig := newDynamicRig(t, []DynamicRoute{r})
	if n := PublicLimiterSize(rig.h); n != 0 {
		t.Fatalf("an unused limiter holds %d", n)
	}
	rig.do("", http.MethodGet, "/api/v1/x/acme_fake/hook", "")
	if n := PublicLimiterSize(rig.h); n != 1 || PublicLimiterSize(http.NotFoundHandler()) != 0 {
		t.Fatalf("after one public call the limiter holds %d", n)
	}
}
