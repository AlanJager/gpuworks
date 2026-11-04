package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// fakeExperience is the launcher as the app wires it, without a process.
type fakeExperience struct {
	mu       sync.Mutex
	on       bool
	password string
	calls    []string
	err      error
}

func (f *fakeExperience) Status(context.Context) dto.ExperienceStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := dto.ExperienceStatus{Enabled: f.on, Ready: f.on, Port: 8810, User: "admin"}
	if f.on {
		st.Password = f.password
	}
	return st
}

func (f *fakeExperience) act(name string, apply func()) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.err != nil {
		return f.err
	}
	apply()
	return nil
}

func (f *fakeExperience) SetEnabled(_ context.Context, on bool) error {
	name := "off"
	if on {
		name = "on"
	}
	return f.act(name, func() { f.on, f.password = on, "p-"+name })
}

func (f *fakeExperience) Reset(context.Context) error {
	return f.act("reset", func() {})
}

func (f *fakeExperience) RotatePassword(context.Context) error {
	return f.act("password", func() { f.password = "rotated" })
}

func experienceRun(t *testing.T, e Experience) *imageManifestRun {
	t.Helper()
	run := newImageManifestRun(t)
	run.deps.Experience = e
	run.router = Router(run.deps)
	return run
}

func TestExperienceRoutes(t *testing.T) {
	exp := &fakeExperience{}
	run := experienceRun(t, exp)

	// Off: no password on the wire.
	rec := run.do(http.MethodGet, "/api/v1/experience", "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":false,"ready":false,"port":8810,"user":"admin"}` {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	rec = run.do(http.MethodPut, "/api/v1/experience/enabled", "application/json", `{"enabled":true}`)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"enabled":true,"ready":true,"port":8810,"user":"admin","password":"p-on"}` {
		t.Fatalf("switch on = %d %s", rec.Code, rec.Body.String())
	}
	rec = run.do(http.MethodPost, "/api/v1/experience/password", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"password":"rotated"`) {
		t.Fatalf("rotate = %d %s", rec.Code, rec.Body.String())
	}
	if rec := run.do(http.MethodPost, "/api/v1/experience/reset", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body.String())
	}
	if rec := run.do(http.MethodPut, "/api/v1/experience/enabled", "application/json", `{"enabled":false}`); rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("switch off = %d %s", rec.Code, rec.Body.String())
	}
	if got := strings.Join(exp.calls, ","); got != "on,password,reset,off" {
		t.Fatalf("calls = %s", got)
	}

	for _, body := range []string{`{}`, `{"enabled":"yes"}`, `nope`, `{"enabled":null}`} {
		rec := run.do(http.MethodPut, "/api/v1/experience/enabled", "application/json", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorExperienceInvalid+`"`) {
			t.Fatalf("PUT %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if len(exp.calls) != 4 {
		t.Fatalf("rejected requests reached the launcher: %v", exp.calls)
	}

	exp.err = errors.New("port 8810 is not available")
	for _, req := range []struct{ method, path, body string }{
		{http.MethodPut, "/api/v1/experience/enabled", `{"enabled":true}`},
		{http.MethodPost, "/api/v1/experience/reset", ""},
		{http.MethodPost, "/api/v1/experience/password", ""},
	} {
		rec := run.do(req.method, req.path, "application/json", req.body)
		var body struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusConflict ||
			body.Code != ErrorExperienceFailed || body.Params["reason"] != "port 8810 is not available" {
			t.Fatalf("failed %s %s = %d %s", req.method, req.path, rec.Code, rec.Body.String())
		}
	}

	if rec := run.do(http.MethodPost, "/api/v1/experience", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", rec.Code)
	}
}

func TestExperienceRoutesNeedAnAdministrator(t *testing.T) {
	exp := &fakeExperience{}
	run := experienceRun(t, exp)
	signer, viewer := roleTestToken(t, run.store, "viewer")
	run.deps.AuthSigner = signer
	run.router = Router(run.deps)
	routes := []struct{ method, path string }{
		{http.MethodGet, "/api/v1/experience"}, {http.MethodPut, "/api/v1/experience/enabled"},
		{http.MethodPost, "/api/v1/experience/reset"}, {http.MethodPost, "/api/v1/experience/password"},
	}
	for _, r := range routes {
		rec := httptest.NewRecorder()
		run.router.ServeHTTP(rec, httptest.NewRequest(r.method, r.path, strings.NewReader(`{"enabled":true}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s %s = %d", r.method, r.path, rec.Code)
		}
		req := httptest.NewRequest(r.method, r.path, strings.NewReader(`{"enabled":true}`))
		req.Header.Set("Authorization", "Bearer "+viewer)
		rec = httptest.NewRecorder()
		run.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("viewer %s %s = %d %s", r.method, r.path, rec.Code, rec.Body.String())
		}
	}
	if len(exp.calls) != 0 {
		t.Fatalf("unauthorized requests reached the launcher: %v", exp.calls)
	}
}

// The experience environment's own console, and a harness, have none.
func TestExperienceRoutesWithoutTheLauncher(t *testing.T) {
	run := newImageManifestRun(t)
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/experience", ""},
		{http.MethodPut, "/api/v1/experience/enabled", `{"enabled":true}`},
		{http.MethodPost, "/api/v1/experience/reset", ""},
		{http.MethodPost, "/api/v1/experience/password", ""},
	} {
		rec := run.do(req.method, req.path, "application/json", req.body)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `"code":"`+ErrorExperienceUnavailable+`"`) {
			t.Fatalf("%s %s = %d %s", req.method, req.path, rec.Code, rec.Body.String())
		}
	}
}

// A console beside another on the same host names its own session cookie,
// so logging into one never logs the other out.
func TestSessionPolicyCookieNamesTheSessionCookie(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "session.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, _, _, err := st.EnsureAdmin("admin", "gpuworks-test-password-2026"); err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner("session-test-secret", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ policy, want string }{{"", managementCookie}, {"gpuworks-experience-session", "gpuworks-experience-session"}} {
		router := Router(Deps{Store: st, Bus: st.Bus, AuthSigner: signer}, auth.SessionPolicy{Cookie: tc.policy})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", strings.NewReader(`{"username":"admin","password":"gpuworks-test-password-2026"}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("login = %d %s", rec.Code, rec.Body.String())
		}
		var session *http.Cookie
		for _, c := range rec.Result().Cookies() {
			if c.Name == tc.want {
				session = c
			} else if c.Name == managementCookie || c.Name == "gpuworks-experience-session" {
				t.Fatalf("policy %q set cookie %q", tc.policy, c.Name)
			}
		}
		if session == nil {
			t.Fatalf("policy %q: no %s cookie in %v", tc.policy, tc.want, rec.Result().Cookies())
		}
		if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", session, ""); rec.Code != http.StatusOK {
			t.Fatalf("policy %q: cookie session = %d", tc.policy, rec.Code)
		}
		// The other console's cookie, of the same value, is not this one's.
		other := &http.Cookie{Name: managementCookie, Value: session.Value}
		if tc.want == managementCookie {
			other.Name = "gpuworks-experience-session"
		}
		if rec := sessionRequest(router, http.MethodGet, "/api/v1/auth/me", other, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("policy %q: foreign cookie name accepted: %d", tc.policy, rec.Code)
		}
		rec = sessionRequest(router, http.MethodPost, "/api/v1/auth/logout", session, "")
		cleared := false
		for _, c := range rec.Result().Cookies() {
			cleared = cleared || (c.Name == tc.want && c.MaxAge < 0)
		}
		if !cleared {
			t.Fatalf("policy %q: logout did not clear %s: %v", tc.policy, tc.want, rec.Result().Cookies())
		}
	}
}
