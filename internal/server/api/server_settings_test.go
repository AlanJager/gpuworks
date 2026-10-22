package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
)

// fakeServerSettings records what the handlers pass through and answers from
// canned values or errors.
type fakeServerSettings struct {
	StaticServerSettings
	listen     dto.ServerListen
	updateErr  error
	restartErr error
	updates    []dto.ServerListenUpdate
	localAddr  string
	restarts   int
}

func (f *fakeServerSettings) Listen() dto.ServerListen { return f.listen }
func (f *fakeServerSettings) UpdateListen(u dto.ServerListenUpdate, local string) (dto.ServerListen, error) {
	f.updates = append(f.updates, u)
	f.localAddr = local
	if f.updateErr != nil {
		return dto.ServerListen{}, f.updateErr
	}
	f.listen.Console, f.listen.Gateway, f.listen.RestartPending = u.Console, u.Gateway, true
	return f.listen, nil
}
func (f *fakeServerSettings) Restart() error {
	f.restarts++
	return f.restartErr
}

func serverSettingsRouter(t *testing.T, fake *fakeServerSettings) (http.Handler, string, string) {
	t.Helper()
	signer, err := auth.NewTokenSigner("server-settings-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st := managementSessionTestStore(t)
	admin, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	viewer, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 2, Username: "viewer", Role: "viewer"})
	return Router(Deps{Store: st, AuthSigner: signer, Server: fake}), admin, viewer
}

func serverSettingsCall(r http.Handler, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 8800}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestServerListenRoutesNeedAnAdministrator(t *testing.T) {
	fake := &fakeServerSettings{}
	r, admin, viewer := serverSettingsRouter(t, fake)
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/server/listen"}, {http.MethodPut, "/api/v1/server/listen"}, {http.MethodPost, "/api/v1/server/restart"},
	} {
		if rec := serverSettingsCall(r, c.method, c.path, "", "{}"); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d", c.method, c.path, rec.Code)
		}
		if rec := serverSettingsCall(r, c.method, c.path, viewer, "{}"); rec.Code != http.StatusForbidden {
			t.Errorf("%s %s as a viewer = %d", c.method, c.path, rec.Code)
		}
	}
	if len(fake.updates) != 0 || fake.restarts != 0 {
		t.Fatalf("an unauthorized request reached the settings: %+v %d", fake.updates, fake.restarts)
	}
	if rec := serverSettingsCall(r, http.MethodDelete, "/api/v1/server/listen", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous DELETE = %d", rec.Code)
	}
	if rec := serverSettingsCall(r, http.MethodDelete, "/api/v1/server/listen", admin, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("authenticated DELETE = %d", rec.Code)
	}
}

func TestServerListenWireFormat(t *testing.T) {
	fake := &fakeServerSettings{listen: dto.ServerListen{
		Console: []string{"10.0.0.1:8800"}, Agent: []string{}, Gateway: []string{},
		Running:    []dto.ServerBinding{{Addr: "10.0.0.1:8800", Functions: []string{"console", "agent", "gateway"}}},
		ConfigPath: "/opt/gpuworks/server/server.yaml", Editable: true, RestartMode: "command",
		RestartCommand: "gpuworks restart --dir /opt/gpuworks/server",
	}}
	r, admin, _ := serverSettingsRouter(t, fake)
	rec := serverSettingsCall(r, http.MethodGet, "/api/v1/server/listen", admin, "")
	want := `{"console":["10.0.0.1:8800"],"agent":[],"gateway":[],"running":[{"addr":"10.0.0.1:8800","functions":["console","agent","gateway"]}],"restart_pending":false,"config_path":"/opt/gpuworks/server/server.yaml","editable":true,"overridden":false,"restart_mode":"command","restart_command":"gpuworks restart --dir /opt/gpuworks/server"}`
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("GET = %d\n%s\nwant\n%s", rec.Code, rec.Body.String(), want)
	}

	rec = serverSettingsCall(r, http.MethodPut, "/api/v1/server/listen", admin, `{"console":["10.0.0.1:8800"],"agent":[],"gateway":["10.0.0.2:443"]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"restart_pending":true`) {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body.String())
	}
	got := fake.updates[0]
	if got.Gateway[0] != "10.0.0.2:443" || fake.localAddr != "10.0.0.1:8800" {
		t.Fatalf("update = %+v via %q", got, fake.localAddr)
	}
}

func TestServerListenErrorsAreProductErrors(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{&ServerSettingsError{Code: ErrorServerListenLockout, Fallback: "lockout", Params: map[string]any{"address": "10.0.0.1:8800"}}, 400, ErrorServerListenLockout},
		{&ServerSettingsError{Code: ErrorServerListenNotLocal, Fallback: "not local", Params: map[string]any{"address": "1.2.3.4:1"}}, 400, ErrorServerListenNotLocal},
		{&ServerSettingsError{Code: ErrorServerListenReadOnly, Fallback: "read only"}, 409, ErrorServerListenReadOnly},
		{errors.New("disk full"), 500, ""},
	} {
		fake := &fakeServerSettings{updateErr: c.err}
		r, admin, _ := serverSettingsRouter(t, fake)
		rec := serverSettingsCall(r, http.MethodPut, "/api/v1/server/listen", admin, `{"console":["10.0.0.1:8800"]}`)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || (c.code != "" && body["code"] != c.code) {
			t.Fatalf("%v -> %d %s", c.err, rec.Code, rec.Body.String())
		}
	}
	fake := &fakeServerSettings{}
	r, admin, _ := serverSettingsRouter(t, fake)
	if rec := serverSettingsCall(r, http.MethodPut, "/api/v1/server/listen", admin, `{"console":`); rec.Code != 400 || !strings.Contains(rec.Body.String(), ErrorServerListenInvalid) {
		t.Fatalf("malformed body = %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.updates) != 0 {
		t.Fatal("a malformed body reached the settings")
	}
}

func TestServerRestartAnswersAcceptedOrExplainsTheCommand(t *testing.T) {
	fake := &fakeServerSettings{}
	r, admin, _ := serverSettingsRouter(t, fake)
	if rec := serverSettingsCall(r, http.MethodPost, "/api/v1/server/restart", admin, ""); rec.Code != http.StatusAccepted || strings.TrimSpace(rec.Body.String()) != `{"status":"restarting"}` || fake.restarts != 1 {
		t.Fatalf("restart = %d %s", rec.Code, rec.Body.String())
	}
	fake.restartErr = &ServerSettingsError{Code: ErrorServerRestartManual, Fallback: "run it", Params: map[string]any{"command": "gpuworks restart"}}
	rec := serverSettingsCall(r, http.MethodPost, "/api/v1/server/restart", admin, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"command":"gpuworks restart"`) {
		t.Fatalf("manual restart = %d %s", rec.Code, rec.Body.String())
	}
	// Without configured settings the API still answers, read-only.
	st := managementSessionTestStore(t)
	signer, err := auth.NewTokenSigner("server-settings-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	admin, err = managementSessionTestToken(t, st, signer, auth.Claims{Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	plain := Router(Deps{Store: st, AuthSigner: signer})
	if rec := serverSettingsCall(plain, http.MethodPut, "/api/v1/server/listen", admin, `{"console":["10.0.0.1:8800"]}`); rec.Code != http.StatusConflict {
		t.Fatalf("static settings update = %d %s", rec.Code, rec.Body.String())
	}
}
