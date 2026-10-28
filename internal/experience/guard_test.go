package experience

import (
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/api"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"/api/v1/apikeys/*", "/api/v1/apikeys/abc", true},
		{"/api/v1/apikeys/*", "/api/v1/apikeys/", false},
		{"/api/v1/apikeys/*", "/api/v1/apikeys/a/b", false},
		{"/api/v1/apikeys", "/api/v1/apikeys/x", false},
		{"/api/v1/playground/v1/**", "/api/v1/playground/v1/chat/completions", true},
		{"/api/v1/playground/v1/**", "/api/v1/playground/v1", false},
		{"/api/v1/deployments/*/stop", "/api/v1/deployments/3/stop", true},
		{"/api/v1/deployments/*/stop", "/api/v1/deployments/3/start", false},
	}
	for _, c := range cases {
		if got := matchPath(c.pattern, c.path); got != c.want {
			t.Errorf("matchPath(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

// guarded is g in front of a console that echoes what reached it.
func guarded(g *guard) http.Handler {
	return g.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Body", string(body))
		w.Header().Set("X-Internal", r.Header.Get(internalHeader))
		w.WriteHeader(http.StatusTeapot)
	}))
}

func serve(t *testing.T, h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestGuardPermitsAndRefuses(t *testing.T) {
	g := newGuard("s3cret")
	h := guarded(g)
	allowed := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/nodes"},
		{http.MethodGet, "/api/v1/deployments/1"},
		{http.MethodPost, "/api/v1/auth/login"},
		{http.MethodPost, "/api/v1/models"},
		{http.MethodPut, "/api/v1/deployments/4/replicas"},
		{http.MethodDelete, "/api/v1/apikeys/abc/"},
		{http.MethodPost, "/api/v1/playground/v1/chat/completions"},
		{http.MethodPost, "/v1/chat/completions"},
		{http.MethodGet, "/api/v1/agent/connect"},
		{http.MethodPost, "/api/v1/agent/connect"},
	}
	for _, c := range allowed {
		if rec := serve(t, h, c.method, c.path, "{}", nil); rec.Code != http.StatusTeapot {
			t.Errorf("%s %s: status %d, want it passed through", c.method, c.path, rec.Code)
		}
	}
	refused := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/nodes/install"},
		{http.MethodPost, "/api/v1/nodes/precheck"},
		{http.MethodPost, "/api/v1/providers/1/sync"},
		{http.MethodPost, "/api/v1/providers/1/install"},
		{http.MethodPut, "/api/v1/server/listen"},
		{http.MethodPost, "/api/v1/server/restart"},
		{http.MethodDelete, "/api/v1/nodes/abc"},
		{http.MethodPost, "/api/v1/storages"},
		{http.MethodGet, "/api/v1/nodes/abc/terminal"},
		{http.MethodGet, "/api/v1/nodes/abc/fs"},
		{http.MethodGet, "/api/v1/storages/2/files"},
		{http.MethodGet, "/api/v1/storages/2/files/download"},
		{http.MethodGet, "/install.sh"},
		{http.MethodHead, "/install.sh"},
	}
	for _, c := range refused {
		rec := serve(t, h, c.method, c.path, "{}", nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: status %d, want 403", c.method, c.path, rec.Code)
			continue
		}
		if c.method == http.MethodHead {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["code"] != "EXPERIENCE_UNSUPPORTED" || body["error"] == "" {
			t.Errorf("%s %s: body %s", c.method, c.path, rec.Body.String())
		}
	}
}

func TestGuardInternalSecret(t *testing.T) {
	g := newGuard("s3cret")
	h := guarded(g)
	rec := serve(t, h, http.MethodPost, "/api/v1/nodes/install", "{}", map[string]string{internalHeader: "s3cret"})
	if rec.Code != http.StatusTeapot {
		t.Fatalf("the environment's own request was refused: %d", rec.Code)
	}
	rec = serve(t, h, http.MethodPost, "/api/v1/nodes/install", "{}", map[string]string{internalHeader: "wrong"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a wrong secret passed: %d", rec.Code)
	}
	rec = serve(t, h, http.MethodGet, "/api/v1/nodes", "", map[string]string{internalHeader: "wrong"})
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Internal") != "" {
		t.Fatalf("a visitor's header reached the console: %d %q", rec.Code, rec.Header().Get("X-Internal"))
	}
}

func TestGuardDeploysOnlyPreloadedModels(t *testing.T) {
	g := newGuard("s3cret")
	g.allowModels(map[uint]bool{1: true}, []string{"Qwen/Qwen3-8B"})
	h := guarded(g)
	rec := serve(t, h, http.MethodPost, "/api/v1/deployments", `{"model_id":2}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403", rec.Code)
	}
	want := `{"code":"EXPERIENCE_MODEL_NOT_PRELOADED","error":"the experience environment deploys only its preloaded models: Qwen/Qwen3-8B","params":{"models":"Qwen/Qwen3-8B"}}`
	if got := strings.TrimSpace(rec.Body.String()); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	body := `{"model_id":1,"name":"x"}`
	rec = serve(t, h, http.MethodPost, "/api/v1/deployments", body, nil)
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Body") != body {
		t.Fatalf("preloaded deploy: status %d, body seen %q", rec.Code, rec.Header().Get("X-Body"))
	}
	rec = serve(t, h, http.MethodPost, "/api/v1/deployments", `not json`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("malformed body: status %d, want 403", rec.Code)
	}
	// The environment's own seeding deploys anything.
	rec = serve(t, h, http.MethodPost, "/api/v1/deployments", `{"model_id":2}`, map[string]string{internalHeader: "s3cret"})
	if rec.Code != http.StatusTeapot {
		t.Fatalf("internal deploy: status %d", rec.Code)
	}
}

// TestGuardPatternsNameConsoleRoutes keeps the guard's lists in step with
// the console's routes: a pattern naming no route guards nothing.
func TestGuardPatternsNameConsoleRoutes(t *testing.T) {
	routes := map[string]bool{}
	router, ok := api.Router(api.Deps{PlaygroundV1: http.NotFoundHandler(), InstallSh: http.NotFound}).(chi.Routes)
	if !ok {
		t.Fatal("the console router cannot be walked")
	}
	if err := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(route, "*")
		route = strings.TrimSuffix(route, "/")
		routes[method+" "+route], routes["* "+route] = true, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{allowedWrites, refusedReads} {
		for _, p := range list {
			p = strings.Replace(p, "/**", "/*", 1)
			_, path, _ := strings.Cut(p, " ")
			// A route mounted with Handle answers every method.
			if !routes[p] && !routes["* "+path] {
				t.Errorf("guard pattern %q names no console route", p)
			}
		}
	}
}

func TestGuardEvaluatesOnlyOwnDeploymentsFromTheConsole(t *testing.T) {
	h := guarded(newGuard("secret"))
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"target_kind":"instance","deployment_id":1}`, http.StatusTeapot},
		{`{"target_kind":"instance","load_origin":"server"}`, http.StatusTeapot},
		{`{"target_kind":"external","base_url":"http://169.254.169.254/"}`, http.StatusForbidden},
		{`{"base_url":"http://10.0.0.1/"}`, http.StatusForbidden},
		{`{"target_kind":"instance","load_origin":"node-uuid"}`, http.StatusForbidden},
	} {
		if rec := serve(t, h, http.MethodPost, "/api/v1/benchmarks", tc.body, nil); rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
	for _, path := range []string{"/api/v1/benchmarks/probe", "/api/v1/benchmarks/models"} {
		if rec := serve(t, h, http.MethodPost, path, `{"url":"http://169.254.169.254/"}`, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", path, rec.Code)
		}
	}
}

func TestGuardRefusesEscapedPathsAndLargeBodies(t *testing.T) {
	h := guarded(newGuard("secret"))
	if rec := serve(t, h, http.MethodGet, "/api/v1/nodes/a%2Fb/fs", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("escaped path: status %d, want 403", rec.Code)
	}
	big := `{"name":"` + strings.Repeat("x", maxWriteBody) + `"}`
	if rec := serve(t, h, http.MethodPost, "/api/v1/benchmarks", big, nil); rec.Code != http.StatusForbidden {
		t.Errorf("oversized write: status %d, want 403", rec.Code)
	}
}
