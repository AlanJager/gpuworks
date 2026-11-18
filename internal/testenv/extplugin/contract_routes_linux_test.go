//go:build linux

package extplugin

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
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
