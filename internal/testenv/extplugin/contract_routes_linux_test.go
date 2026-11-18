//go:build linux

package extplugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// withFakeRoutes declares the fake's routes: an administrator's list and
// create, a member's read and a public home page.
func withFakeRoutes(s *extsign.Spec) {
	m := &s.Manifest
	m.Requires[extapi.CapRoutes] = 1
	m.Permissions = append(m.Permissions, "http:public", "http:routes")
	slices.Sort(m.Permissions)
	m.Routes = []extapi.RouteDecl{
		{Method: "GET", Pattern: "/jobs", Action: "acme_fake.jobs_list", Access: extapi.RouteAdmin, Ungated: true},
		{Method: "POST", Pattern: "/jobs", Action: "acme_fake.jobs_create", Access: extapi.RouteAdminSession, Ungated: true},
		{Method: "GET", Pattern: "/jobs/{id}", Action: "acme_fake.jobs_get", Access: extapi.RouteMember, Ungated: true},
		{Method: "GET", Pattern: "/", Action: "acme_fake.home", Access: extapi.RoutePublic, Ungated: true},
	}
}

// pluginLines are the route inventory's lines for package id.
func pluginLines(t *testing.T, sv *server, id string) []string {
	t.Helper()
	all, err := sv.a.RouteInventory()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range all {
		if strings.Contains(line, "[x="+id+"@") {
			out = append(out, line)
		}
	}
	return out
}

func routeCalls(s *site, id string) []result {
	var out []result
	for _, r := range s.results(id) {
		if r.Route != "" {
			out = append(out, r)
		}
	}
	return out
}

// A plugin installed while the server runs answers its routes at once,
// signed for the caller and the route it matched; a path it does not
// declare never reaches it; disabled or uninstalled its routes are the
// core's 404, and while it crashloops they answer 503 and the plugin
// administration and sign-in keep working.
func TestContractPluginRoutesAtRuntime(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	s.args("acme_fake", "--routes")
	sv.install(s.fakePackage("acme_fake", "1.0.0", withFakeRoutes), true)
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })

	for _, tc := range []struct {
		method, path, token, route, kind string
	}{
		{"GET", "/api/v1/x/acme_fake/jobs", sv.token, "acme_fake.jobs_list", "session"},
		{"POST", "/api/v1/x/acme_fake/jobs", sv.token, "acme_fake.jobs_create", "session"},
		{"GET", "/api/v1/x/acme_fake/jobs/7", sv.token, "acme_fake.jobs_get", "session"},
		{"GET", "/api/v1/x/acme_fake/", "", "acme_fake.home", "public"},
	} {
		token := sv.token
		sv.token = tc.token
		status, body := sv.do(tc.method, tc.path, nil)
		sv.token = token
		var got result
		if status != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Route != tc.route || got.Kind != tc.kind {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, status, body)
		}
		if tc.kind == "session" && got.User != "admin" {
			t.Fatalf("%s %s: the plugin saw user %q", tc.method, tc.path, got.User)
		}
	}
	served := len(routeCalls(s, "acme_fake"))
	if status, _ := sv.do("GET", "/api/v1/x/acme_fake/nope", nil); status != http.StatusNotFound {
		t.Fatalf("an undeclared path = %d", status)
	}
	if status, _ := sv.do("GET", "/api/v1/x/acme_fake/jobs/7/../../jobs", nil); status != http.StatusBadRequest {
		t.Fatalf("a non-canonical path = %d", status)
	}
	if n := len(routeCalls(s, "acme_fake")); served != 4 || n != served {
		t.Fatalf("the plugin served %d calls, then %d", served, n)
	}
	want := []string{
		"GET /api/v1/x/acme_fake/ [access=public] [action=acme_fake.home] [ungated] [x=acme_fake@1.0.0]",
		"GET /api/v1/x/acme_fake/jobs [access=admin] [action=acme_fake.jobs_list] [ungated] [x=acme_fake@1.0.0]",
		"GET /api/v1/x/acme_fake/jobs/{id} [access=member] [action=acme_fake.jobs_get] [ungated] [x=acme_fake@1.0.0]",
		"POST /api/v1/x/acme_fake/jobs [access=admin_session] [action=acme_fake.jobs_create] [ungated] [x=acme_fake@1.0.0]",
	}
	if got := pluginLines(t, sv, "acme_fake"); !slices.Equal(got, want) {
		t.Fatalf("inventory lines:\n%s", strings.Join(got, "\n"))
	}

	// Disabled: the core's 404, and no line in the inventory.
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/disable", nil); status != http.StatusOK {
		t.Fatalf("disable = %d %s", status, body)
	}
	if status, _ := sv.do("GET", "/api/v1/x/acme_fake/jobs", nil); status != http.StatusNotFound || len(pluginLines(t, sv, "acme_fake")) != 0 {
		t.Fatalf("a disabled plugin's route = %d, inventory %v", status, pluginLines(t, sv, "acme_fake"))
	}

	// Crashlooping: 503 with a retry, and the administration stays up.
	s.args("acme_fake", "--routes", "--exit-at-start", "3")
	if status, body := sv.do(http.MethodPost, "/api/v1/plugins/acme_fake/enable", nil); status != http.StatusOK {
		t.Fatalf("enable = %d %s", status, body)
	}
	var code string
	waitFor(t, "a 503 from the crashing plugin's route", func() bool {
		req, _ := http.NewRequest(http.MethodGet, sv.url+"/api/v1/x/acme_fake/jobs", nil)
		req.Header.Set("Authorization", "Bearer "+sv.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var body struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		code = body.Code
		return resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("Retry-After") == "5"
	})
	if code != "PLUGIN_UNAVAILABLE" {
		t.Fatalf("crashloop code = %s", code)
	}
	if status, _ := sv.do(http.MethodGet, "/api/v1/plugins", nil); status != http.StatusOK {
		t.Fatalf("plugins list while the plugin crashes = %d", status)
	}
	if status, body := sv.do(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`)); status != http.StatusOK {
		t.Fatalf("login while the plugin crashes = %d %s", status, body)
	}

	// Uninstalled: the core's 404.
	if status, body := sv.do(http.MethodDelete, "/api/v1/plugins/acme_fake", nil); status != http.StatusNoContent {
		t.Fatalf("uninstall = %d %s", status, body)
	}
	if status, _ := sv.do("GET", "/api/v1/x/acme_fake/jobs", nil); status != http.StatusNotFound || len(pluginLines(t, sv, "acme_fake")) != 0 {
		t.Fatalf("an uninstalled plugin's route = %d", status)
	}
}

// What a real plugin answers passes only within its route's rules: HTML
// from a route that does not declare html is refused, and a public
// cookies route's cookies reach the client held to the plugin's prefix,
// without a core cookie.
func TestContractPluginRouteResponsesAtRuntime(t *testing.T) {
	s := newSite(t)
	sv := s.boot()
	s.args("acme_fake", "--routes", "--html", "--set-cookie", "gpuworks_session=x; Path=/", "--set-cookie", "sso=1; Path=/")
	sv.install(s.fakePackage("acme_fake", "1.0.0", func(spec *extsign.Spec) {
		withFakeRoutes(spec)
		spec.Manifest.Routes = append(spec.Manifest.Routes, extapi.RouteDecl{Method: "GET", Pattern: "/sso", Action: "acme_fake.sso",
			Access: extapi.RoutePublic, Ungated: true, HTML: true, Cookies: true})
	}), true)
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })

	status, body := sv.do("GET", "/api/v1/x/acme_fake/jobs", nil)
	if want := `{"code":"PLUGIN_ROUTE_RESPONSE_INVALID","error":"the plugin returned a response the server will not pass on: the route does not declare html","params":{"id":"acme_fake"}}`; status != http.StatusBadGateway || strings.TrimSpace(string(body)) != want {
		t.Fatalf("html from a route without html = %d %s", status, body)
	}

	resp, err := http.Get(sv.url + "/api/v1/x/acme_fake/sso")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Values("Set-Cookie"); resp.StatusCode != http.StatusOK || !slices.Equal(got, []string{"sso=1; Path=/api/v1/x/acme_fake/"}) ||
		resp.Header.Get("Content-Type") != "text/html; charset=utf-8" || resp.Header.Get("Content-Security-Policy") != "sandbox; default-src 'none'" {
		t.Fatalf("public cookies route = %d, cookies %q, headers %v", resp.StatusCode, got, resp.Header)
	}
	if n := len(routeCalls(s, "acme_fake")); n != 2 {
		t.Fatalf("the plugin served %d calls, want 2", n)
	}
}

// A real plugin process that stops reading a route's request body is
// restarted with PLUGIN_STALLED once its connection has made no progress
// for StallAfter, though it keeps answering health probes; the stuck call
// answers 502 PLUGIN_ROUTE_FAILED at once instead of waiting out the
// route's timeout.
func TestContractPluginThatStopsReadingBodiesIsRestarted(t *testing.T) {
	s := newSite(t)
	s.timing = func(tm *pluginhost.Timing) {
		tm.StallAfter = time.Second
		// A backoff long enough to read the reason from the plugin list.
		tm.BackoffMin, tm.BackoffMax = 3*time.Second, 3*time.Second
	}
	sv := s.boot()
	s.args("acme_fake", "--routes", "--stall-bodies")
	sv.install(s.fakePackage("acme_fake", "1.0.0", func(spec *extsign.Spec) {
		withFakeRoutes(spec)
		m := &spec.Manifest
		m.Permissions = append(m.Permissions, "http:uploads")
		slices.Sort(m.Permissions)
		m.Routes = append(m.Routes, extapi.RouteDecl{Method: "POST", Pattern: "/uploads", Action: "acme_fake.upload",
			Access: extapi.RouteAdmin, Ungated: true, MaxBody: 16 << 20})
	}), true)
	sv.waitPlugin("acme_fake", "running", func(p dto.Plugin) bool { return p.State == "running" })

	type answer struct {
		status int
		body   []byte
		took   time.Duration
	}
	answered := make(chan answer, 1)
	go func() {
		started := time.Now()
		status, body := sv.do(http.MethodPost, "/api/v1/x/acme_fake/uploads", bytes.NewReader(make([]byte, 8<<20)))
		answered <- answer{status, body, time.Since(started)}
	}()
	p := sv.waitPlugin("acme_fake", "restarted for a stall", func(p dto.Plugin) bool {
		return p.State == "backoff" && p.Message != nil && p.Message.Code == "PLUGIN_STALLED"
	})
	if p.Message.Params["id"] != "acme_fake" || p.Message.Params["stalled_s"] != float64(1) {
		t.Fatalf("stall message = %+v", p.Message)
	}
	a := <-answered
	var refusal struct {
		Code string `json:"code"`
	}
	if a.status != http.StatusBadGateway || json.Unmarshal(a.body, &refusal) != nil || refusal.Code != "PLUGIN_ROUTE_FAILED" {
		t.Fatalf("the stalled call = %d %s", a.status, a.body)
	}
	if a.took > 10*time.Second {
		t.Fatalf("the stalled call took %v", a.took)
	}
	p = sv.waitPlugin("acme_fake", "running again", func(p dto.Plugin) bool { return p.State == "running" })
	if p.Restarts != 1 {
		t.Fatalf("restarts = %d, want 1", p.Restarts)
	}

	// The restarted plugin still answers calls without a body.
	if status, body := sv.do(http.MethodGet, "/api/v1/x/acme_fake/jobs", nil); status != http.StatusOK {
		t.Fatalf("a call without a body = %d %s", status, body)
	}
	if p := sv.plugin("acme_fake"); p.Restarts != 1 || p.State != "running" {
		t.Fatalf("after a call without a body: %+v", p)
	}
}
