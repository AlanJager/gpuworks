package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// switchLimiter is a PlatformLimiter a test switches between answers.
type switchLimiter struct{ cur atomic.Pointer[PlatformLimit] }

func newSwitchLimiter(access featuregate.Access) *switchLimiter {
	l := &switchLimiter{}
	l.set(PlatformLimit{Access: access})
	return l
}

func (l *switchLimiter) set(limit PlatformLimit)      { l.cur.Store(&limit) }
func (l *switchLimiter) PlatformLimit() PlatformLimit { return *l.cur.Load() }

// fullRouter is the router with every optional mount present, so the
// inventory holds every route a limit can meet.
func fullRouter(extra ...Extension) http.Handler {
	nop := func(http.ResponseWriter, *http.Request) {}
	return Router(Deps{AgentConnect: nop, InstallSh: nop, ProxyV1: http.HandlerFunc(nop), UIHandler: http.HandlerFunc(nop),
		Extensions: ExtensionDeps{Extensions: extra}})
}

// inventoryRoutes are the method and pattern of every route of h.
func inventoryRoutes(t *testing.T, h http.Handler) [][2]string {
	t.Helper()
	lines, err := RouteInventory(h)
	if err != nil {
		t.Fatal(err)
	}
	var out [][2]string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			t.Fatalf("inventory line %q has no method and pattern", line)
		}
		out = append(out, [2]string{fields[0], fields[1]})
	}
	return out
}

// A route nobody classified is refused under any limit; this fails until a
// new route outside /api/v1 is named in platform_limit.go, so adding one is
// a decision and never an accident.
func TestEveryRouteIsClassifiedForAPlatformLimit(t *testing.T) {
	seen := map[string]bool{}
	for _, route := range inventoryRoutes(t, fullRouter()) {
		method, pattern := route[0], route[1]
		seen[method+" "+pattern] = true
		// A mount that takes every method is listed once per method.
		if _, classified := classifyPlatformRoute(method, pattern, pattern, ""); !classified {
			t.Errorf("%s %s is not classified for a platform limit: name it in platform_limit.go", method, pattern)
		}
	}
	for _, table := range []map[string]bool{platformRecoveryRoutes, platformChangingReads, platformReadingPosts} {
		for key := range table {
			if !seen[key] {
				t.Errorf("platform_limit.go names %q, which is not a route", key)
			}
		}
	}
	for _, prefix := range platformRecoveryPrefixes {
		found := false
		for key := range seen {
			pattern := key[strings.Index(key, " ")+1:]
			if pattern == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(pattern, strings.TrimSuffix(prefix, "/")+"/") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("platform_limit.go names the subtree %q, which holds no route", prefix)
		}
	}
}

// The routes a limit treats other than by their method are this list. A
// change here widens or narrows what a limited server serves: make it
// deliberately.
func TestPlatformLimitExceptionsAreTheKnownOnes(t *testing.T) {
	var got []string
	for _, route := range inventoryRoutes(t, fullRouter()) {
		method, pattern := route[0], route[1]
		class, _ := classifyPlatformRoute(method, pattern, pattern, "")
		byMethod := platformChange
		switch method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			byMethod = platformRead
		}
		if class == byMethod && class != platformServe {
			continue
		}
		name := map[platformClass]string{platformChange: "change", platformRead: "read", platformServe: "serve", platformRecovery: "recovery"}[class]
		got = append(got, name+" "+method+" "+pattern)
	}
	sort.Strings(got)
	got = slices.Compact(got)
	want := strings.Split(strings.TrimSpace(platformLimitExceptions), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("the routes a platform limit treats specially changed; update the list deliberately\ngot:\n%s", strings.Join(got, "\n"))
	}
}

func TestPlatformLimitServesByClass(t *testing.T) {
	ext := fakeExtension{name: "owner", routes: []RouteSpec{
		{Method: http.MethodGet, Pattern: "/x/owner/state", Community: true},
		{Method: http.MethodPost, Pattern: "/x/owner/import", Access: AccessAdmin, Community: true},
	}}
	other := fakeExtension{name: "other", routes: []RouteSpec{{Method: http.MethodGet, Pattern: "/x/other/things", Community: true}}}
	limiter := newSwitchLimiter(featuregate.Full)
	served := 0
	rig := newTokenRig(t, func(d *Deps) {
		d.Extensions = ExtensionDeps{Extensions: []Extension{ext, other}, Platform: limiter, PlatformOwner: "owner"}
		d.ProxyV1 = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { served++; w.WriteHeader(http.StatusNoContent) })
		d.AgentConnect = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
		d.UIHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
	})
	t.Cleanup(func() { _ = rig.st.Close() })
	read := rig.mint("read")

	const refused = http.StatusForbidden
	for _, tc := range []struct {
		name, bearer, method, path string
		full, readOnly, locked     int
	}{
		{"a read", rig.session, "GET", "/api/v1/nodes", 200, 200, refused},
		{"a read with a read token", read, "GET", "/api/v1/nodes", 200, 200, refused},
		{"a change", rig.session, "PUT", "/api/v1/nodes/n1/labels", 400, refused, refused},
		{"a change without a session", "", "PUT", "/api/v1/nodes/n1/labels", 401, refused, refused},
		{"a read that opens a session on a node", rig.session, "GET", "/api/v1/nodes/n1/terminal", 503, refused, refused},
		{"the data plane", "", "POST", "/v1/chat/completions", 204, 204, refused},
		{"a joined machine's connection", "", "GET", "/api/v1/agent/connect", 204, 204, 204},
		{"who am I", rig.session, "GET", "/api/v1/auth/me", 200, 200, 200},
		{"the console shell", rig.session, "GET", "/api/v1/console/manifest", 200, 200, 200},
		{"signing in", "", "POST", "/api/v1/auth/login", 400, 400, 400},
		{"the limiting extension's read", rig.session, "GET", "/api/v1/x/owner/state", 200, 200, 200},
		{"the limiting extension's change", rig.session, "POST", "/api/v1/x/owner/import", 200, 200, 200},
		{"the limiting extension's unknown route", rig.session, "GET", "/api/v1/x/owner/nothing", 404, 404, 404},
		{"the limiting extension's unknown method", rig.session, "DELETE", "/api/v1/x/owner/state", 405, 405, 405},
		{"another extension's read", rig.session, "GET", "/api/v1/x/other/things", 200, 200, refused},
		{"a core route whose path names the limiting extension", rig.session, "GET", "/api/v1/nodes/owner", 404, 404, refused},
		{"a path that climbs out of the limiting extension", rig.session, "GET", "/api/v1/x/owner/../other/things", 404, 404, 404},
		{"the console's page", "", "GET", "/", 202, 202, 202},
		{"a console page by its address", "", "GET", "/x/owner", 202, 202, 202},
		{"a console file", "", "HEAD", "/assets/index.js", 202, 202, 202},
		{"an agent download", "", "GET", "/dl/gpuworks-agent-linux-amd64", 202, 202, 202},
		{"a write to the catch-all", "", "POST", "/dl/gpuworks-agent-linux-amd64", 202, refused, refused},
		{"no such route", rig.session, "GET", "/api/v1/does-not-exist", 404, 404, 404},
		{"no such method", rig.session, "TRACE", "/api/v1/nodes", 405, 405, 405},
	} {
		for _, state := range []struct {
			access featuregate.Access
			want   int
		}{{featuregate.Full, tc.full}, {featuregate.ReadOnly, tc.readOnly}, {featuregate.Locked, tc.locked}, {featuregate.Hidden, tc.locked}} {
			limiter.set(PlatformLimit{Access: state.access})
			if rec := rig.do(tc.bearer, tc.method, tc.path, ""); rec.Code != state.want {
				t.Errorf("%s under %s: %s %s = %d %s, want %d", tc.name, state.access, tc.method, tc.path, rec.Code, rec.Body.String(), state.want)
			}
		}
	}
	if served == 0 {
		t.Fatal("the data plane was never reached")
	}
}

// The refusal on the wire: the core's own codes, and the limiter's notice
// in their place when it gives one.
func TestPlatformLimitRefusalWire(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Locked)
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Platform: limiter} })
	t.Cleanup(func() { _ = rig.st.Close() })
	body := func(method, path string) string {
		rec := rig.do(rig.session, method, path, "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body.String())
		}
		return strings.TrimSpace(rec.Body.String())
	}
	if got, want := body("GET", "/api/v1/nodes"), `{"code":"PLATFORM_LOCKED","error":"this server is locked; only signing in and the page that unlocks it are served"}`; got != want {
		t.Errorf("locked refusal = %s, want %s", got, want)
	}
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly})
	if got, want := body("POST", "/api/v1/nodes"), `{"code":"PLATFORM_READ_ONLY","error":"this server is read-only; what runs keeps running and can be viewed, changes are refused"}`; got != want {
		t.Errorf("read-only refusal = %s, want %s", got, want)
	}
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly, Notice: messages.NewProductError("ACME_LAPSED", "the acme term ended", map[string]any{"days": 3})})
	if got, want := body("POST", "/api/v1/nodes"), `{"code":"ACME_LAPSED","error":"the acme term ended","params":{"days":3}}`; got != want {
		t.Errorf("notice refusal = %s, want %s", got, want)
	}
}

func TestConsoleManifestShowsThePlatformLimit(t *testing.T) {
	limiter := newSwitchLimiter(featuregate.Full)
	rig := newTokenRig(t, func(d *Deps) { d.Extensions = ExtensionDeps{Platform: limiter} })
	t.Cleanup(func() { _ = rig.st.Close() })
	platform := func() (string, bool) {
		rec := rig.do(rig.session, "GET", "/api/v1/console/manifest", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("manifest = %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		raw, ok := out["platform"]
		return string(raw), ok
	}
	if raw, ok := platform(); ok {
		t.Fatalf("an unlimited server's manifest names a platform limit: %s", raw)
	}
	limiter.set(PlatformLimit{Access: featuregate.Hidden, Link: "/x/acme", Action: messages.NewProductError("ACME_RENEW", "renew", nil)})
	if got, want := first(platform()), `{"access":"locked","link":"/x/acme","action":{"code":"ACME_RENEW","message":"renew"},"message":{"code":"PLATFORM_LOCKED","message":"this server is locked; only signing in and the page that unlocks it are served"}}`; got != want {
		t.Errorf("locked platform = %s, want %s", got, want)
	}
	// An action without a link names nothing and is left out.
	limiter.set(PlatformLimit{Access: featuregate.ReadOnly, Action: messages.NewProductError("ACME_RENEW", "renew", nil),
		Notice: messages.NewProductError("ACME_LAPSED", "the acme term ended", map[string]any{"days": 3})})
	if got, want := first(platform()), `{"access":"read_only","message":{"code":"ACME_LAPSED","params":{"days":3},"message":"the acme term ended"}}`; got != want {
		t.Errorf("read-only platform = %s, want %s", got, want)
	}
}

func first(s string, _ bool) string { return s }

// Without a limiter, and under one that answers Full, the guard resolves
// no route: a nil router would panic if it did.
func TestPlatformGuardAddsNothingWhenNothingIsLimited(t *testing.T) {
	for name, limiter := range map[string]PlatformLimiter{"no limiter": nil, "full": newSwitchLimiter(featuregate.Full)} {
		a := &apiHandlers{}
		a.d.Extensions.Platform = limiter
		reached := 0
		next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached++ })
		a.platformGuard(nil)(next).ServeHTTP(nil, httptestRequest(http.MethodGet, "/api/v1/nodes", ""))
		if reached != 1 {
			t.Errorf("%s: the handler was reached %d times", name, reached)
		}
	}
}

const platformLimitExceptions = `
change GET /api/v1/kube/clusters/{uuid}/manifest
change GET /api/v1/nodes/{uuid}/terminal
change GET /api/v1/slurm/clusters/{uuid}/script
change GET /api/v1/slurm/clusters/{uuid}/setup
change GET /install.sh
change GET /kube/manifest.yaml
change GET /slurm/setup.sh
change OPTIONS /*
read POST /api/v1/mcp
recovery DELETE /api/v1/diagnostics/debug-session
recovery DELETE /api/v1/plugins/overrides/{oid}
recovery DELETE /api/v1/plugins/{id}
recovery GET /*
recovery GET /api/v1/agent/connect
recovery GET /api/v1/auth/me
recovery GET /api/v1/branding
recovery GET /api/v1/console/manifest
recovery GET /api/v1/diagnostics/bundles
recovery GET /api/v1/diagnostics/bundles/{id}
recovery GET /api/v1/diagnostics/bundles/{id}/download
recovery GET /api/v1/diagnostics/debug-session
recovery GET /api/v1/healthz
recovery GET /api/v1/plugins
recovery GET /api/v1/plugins/catalog
recovery GET /api/v1/plugins/overrides
recovery GET /api/v1/plugins/{id}/impact
recovery GET /api/v1/plugins/{id}/logs
recovery GET /api/v1/plugins/{id}/state
recovery GET /api/v1/readyz
recovery GET /api/v1/updates
recovery GET /api/v1/updates/server/result
recovery GET /api/v1/updates/server/versions
recovery GET /healthz
recovery GET /slurm/check.sh
recovery GET /slurm/token-status
recovery HEAD /*
recovery POST /api/v1/auth/activity
recovery POST /api/v1/auth/login
recovery POST /api/v1/auth/logout
recovery POST /api/v1/auth/password
recovery POST /api/v1/diagnostics/bundles
recovery POST /api/v1/diagnostics/debug-session
recovery POST /api/v1/plugins/install
recovery POST /api/v1/plugins/overrides
recovery POST /api/v1/plugins/{id}/disable
recovery POST /api/v1/plugins/{id}/enable
recovery POST /api/v1/plugins/{id}/restart
recovery POST /api/v1/plugins/{id}/rollback
recovery POST /api/v1/plugins/{id}/upgrade
recovery POST /api/v1/slurm/gpu-nodes
recovery POST /api/v1/updates/prefetch
recovery POST /api/v1/updates/server
recovery POST /api/v1/updates/server/upload
recovery PUT /api/v1/updates/auto-check
recovery PUT /api/v1/updates/channel
serve CONNECT /v1/*
serve DELETE /v1/*
serve GET /v1/*
serve HEAD /v1/*
serve OPTIONS /v1/*
serve PATCH /v1/*
serve POST /v1/*
serve PUT /v1/*
serve QUERY /v1/*
serve TRACE /v1/*
`
