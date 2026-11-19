package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// fakePlugins records the calls and answers what the test sets.
type fakePlugins struct {
	mu    sync.Mutex
	calls []string
	body  string
	err   error
	// metricsErr fails WriteMetrics.
	metricsErr error
	// none lists no package.
	none bool
	// follow is closed by a test to wake a logs stream once; the read
	// after it returns the second step of fakeLogs.
	follow   chan struct{}
	logReads int
}

func (f *fakePlugins) Check(context.Context) dto.DiagnosticCheck {
	return dto.DiagnosticCheck{ID: "plugins", Status: "fail", Message: dto.ProductMessage{Code: messages.CheckPluginsFailing,
		Params: map[string]any{"ids": "acme_backup"}, Message: "enabled plugins cannot run: acme_backup"}}
}

func (f *fakePlugins) WriteMetrics(w io.Writer) error {
	if f.metricsErr != nil {
		return f.metricsErr
	}
	_, err := io.WriteString(w, "# TYPE gpuworks_plugin_up gauge\ngpuworks_plugin_up{id=\"acme_backup\"} 1\n")
	return err
}

func (f *fakePlugins) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	return f.err
}

func (f *fakePlugins) List(context.Context) []dto.Plugin {
	_ = f.record("list")
	if f.none {
		return nil
	}
	since := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	return []dto.Plugin{{ID: "acme_backup", Version: "1.0.0", Publisher: "acme", Trust: "host", Enabled: true, State: "running",
		Since: &since, Parts: []string{"server"}, Isolation: "landlock", Roles: []string{}, Permissions: []dto.PluginPermission{{Name: "push:banners", Risk: "low"}},
		ApproveHash: "sha256:00", Previous: "0.9.0", Pending: &dto.PluginPending{Version: "1.1.0"},
		Features: []dto.PluginFeature{{Key: "acme.backup", Availability: "full"}}, Actions: []string{"disable", "restart", "uninstall", "purge"}}}
}

func (f *fakePlugins) Install(_ context.Context, body io.Reader, approve string, enable bool, by string) (dto.PluginInstalled, error) {
	raw, _ := io.ReadAll(body)
	f.mu.Lock()
	f.body = string(raw)
	f.mu.Unlock()
	if err := f.record("install " + approve + " " + map[bool]string{true: "enable", false: "keep"}[enable] + " " + by); err != nil {
		return dto.PluginInstalled{}, err
	}
	return dto.PluginInstalled{Plugin: dto.Plugin{ID: "acme_backup", Version: "1.0.0", State: "starting", Parts: []string{}, Roles: []string{}, Permissions: []dto.PluginPermission{},
		Features: []dto.PluginFeature{}, Actions: []string{}}}, nil
}

func (f *fakePlugins) Enable(_ context.Context, id, by string) (dto.Plugin, error) {
	return dto.Plugin{ID: id, State: "starting"}, f.record("enable " + id + " " + by)
}

func (f *fakePlugins) Disable(_ context.Context, id, by string) (dto.Plugin, error) {
	return dto.Plugin{ID: id, State: "disabled"}, f.record("disable " + id + " " + by)
}

func (f *fakePlugins) Uninstall(_ context.Context, id string, purge bool, confirm, by string) (dto.PluginImpact, error) {
	err := f.record("uninstall " + id + " " + map[bool]string{true: "purge", false: "keep"}[purge] + " " + confirm + " " + by)
	im := fakeImpact(id)
	if err == nil && confirm != im.Confirm {
		err = messages.NewProductError(messages.ErrorPluginUninstallConfirmRequired, "confirm", map[string]any{"id": id, "confirm": im.Confirm,
			"held_nodes": 1, "users_affected": 0, "caps_lifted": 1, "features_locked": 1, "banners": 0, "overrides": 0})
	}
	return im, err
}

func (f *fakePlugins) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type pluginRig struct {
	*tokenRig
	plugins *fakePlugins
	member  string
}

func newPluginRig(t *testing.T, plugins *fakePlugins) *pluginRig {
	t.Helper()
	rig := newTokenRig(t, func(d *Deps) {
		if plugins != nil {
			d.Extensions.Plugins = plugins
		}
	})
	t.Cleanup(func() { _ = rig.st.Close() })
	signer, err := auth.NewTokenSigner("platform-token-test-secret", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	member, err := managementSessionTestToken(t, rig.st, signer, auth.Claims{UserID: 7, Username: "member", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	return &pluginRig{tokenRig: rig, plugins: plugins, member: member}
}

// Who may call each route: nobody unauthenticated, no member, an
// administrator's read or write token only for the list, and an
// administrator's session for everything.
func TestPluginRoutesAccess(t *testing.T) {
	rig := newPluginRig(t, &fakePlugins{})
	readToken, writeToken := rig.mint("read"), rig.mint("write")
	for _, tc := range []struct {
		method, path                 string
		member, read, write, session int
	}{
		{"GET", "/api/v1/plugins", 403, 200, 200, 200},
		{"GET", "/api/v1/metrics/plugins", 403, 200, 200, 200},
		{"POST", "/api/v1/plugins/install?approve=sha256:00", 403, 403, 403, 200},
		{"POST", "/api/v1/plugins/acme_backup/enable", 403, 403, 403, 200},
		{"POST", "/api/v1/plugins/acme_backup/disable", 403, 403, 403, 200},
		{"DELETE", "/api/v1/plugins/acme_backup?confirm=sha256:aa", 403, 403, 403, 204},
		{"DELETE", "/api/v1/plugins/acme_backup", 403, 403, 403, 409},
		{"POST", "/api/v1/plugins/acme_backup/restart", 403, 403, 403, 200},
		{"GET", "/api/v1/plugins/catalog", 403, 200, 200, 200},
		{"GET", "/api/v1/plugins/acme_backup/state", 403, 200, 200, 200},
		{"GET", "/api/v1/plugins/acme_backup/impact", 403, 200, 200, 200},
		{"GET", "/api/v1/plugins/acme_backup/logs", 403, 403, 403, 200},
	} {
		for _, who := range []struct {
			name, token string
			want        int
		}{{"anonymous", "", 401}, {"member", rig.member, tc.member}, {"read token", readToken, tc.read}, {"write token", writeToken, tc.write}, {"session", rig.session, tc.session}} {
			if rec := rig.do(who.token, tc.method, tc.path, "pkg"); rec.Code != who.want {
				t.Errorf("%s %s as %s: %d %s, want %d", tc.method, tc.path, who.name, rec.Code, rec.Body, who.want)
			}
		}
	}
	if rec := rig.do(rig.session, "PUT", "/api/v1/plugins/acme_backup", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PUT a plugin: %d", rec.Code)
	}
}

// The list and the install answer their wire shapes, and every call names
// the administrator for the audit record.
func TestPluginRoutesWire(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	rec := rig.do(rig.session, "GET", "/api/v1/plugins", "")
	want := `{"items":[{"id":"acme_backup","version":"1.0.0","publisher":"acme","trust":"host","enabled":true,"state":"running",` +
		`"since":"2026-11-17T08:00:00Z","restarts":0,"parts":["server"],"isolation":"landlock","roles":[],"permissions":[{"name":"push:banners","risk":"low"}],` +
		`"approve_hash":"sha256:00","previous":"0.9.0","on_demand":false,"pending":{"version":"1.1.0"},` +
		`"features":[{"key":"acme.backup","availability":"full"}],"actions":["disable","restart","uninstall","purge"]}],` +
		`"check":{"id":"plugins","status":"fail","message":{"code":"CHECK_PLUGINS_FAILING","params":{"ids":"acme_backup"},"message":"enabled plugins cannot run: acme_backup"}}}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("list: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	rec = rig.do(rig.session, "POST", "/api/v1/plugins/install?approve=sha256:ab&enable=true", "the package bytes")
	want = `{"plugin":{"id":"acme_backup","version":"1.0.0","enabled":false,"state":"starting","restarts":0,"parts":[],"isolation":"","roles":[],"permissions":[],"on_demand":false,"features":[],"actions":[]},"unchanged":false}`
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("install: %d\n%s\nwant\n%s", rec.Code, rec.Body, want)
	}
	if plugins.body != "the package bytes" {
		t.Fatalf("the package reached the installer as %q", plugins.body)
	}
	rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/enable", "")
	rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/disable", "")
	if rec := rig.do(rig.session, "DELETE", "/api/v1/plugins/acme_backup?purge=true&confirm=sha256:aa", ""); rec.Code != 204 || rec.Body.Len() != 0 {
		t.Fatalf("uninstall: %d %s", rec.Code, rec.Body)
	}
	got := strings.Join(plugins.recorded(), "|")
	if got != "list|install sha256:ab enable admin|enable acme_backup admin|disable acme_backup admin|uninstall acme_backup purge sha256:aa admin" {
		t.Fatalf("calls = %s", got)
	}
}

// Bad query values are refused before the plugin host is asked.
func TestPluginRoutesValidateQueries(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	for _, path := range []string{"/api/v1/plugins/install?enable=maybe", "/api/v1/plugins/acme_backup?purge=2x"} {
		method := "POST"
		if !strings.Contains(path, "install") {
			method = "DELETE"
		}
		rec := rig.do(rig.session, method, path, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if calls := plugins.recorded(); len(calls) != 0 {
		t.Fatalf("the plugin host was asked: %v", calls)
	}
}

// A refusal answers its status and keeps its code and params.
func TestPluginRoutesMapRefusals(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{messages.NewProductError(messages.ErrorPluginNotInstalled, "no", map[string]any{"id": "acme_backup"}), 404},
		{messages.NewProductError(messages.ErrorPluginApprovalRequired, "approve", map[string]any{"id": "acme_backup", "approve_hash": "sha256:11"}), 409},
		{messages.NewProductError(messages.ErrorPluginBroken, "broken", map[string]any{"id": "acme_backup"}), 409},
		{messages.NewProductError(messages.ErrorPluginVersionConflict, "conflict", map[string]any{"id": "acme_backup", "version": "1.0.0"}), 409},
		{messages.NewProductError(messages.ErrorPluginFeatureTaken, "taken", map[string]any{"id": "acme_backup", "feature": "acme.backup", "owner": "acme_vault"}), 409},
		{messages.NewProductError("PLUGIN_STATE_TOO_NEW", "too new", map[string]any{"marker": "plugin.acme_backup.data"}), 409},
		{messages.NewProductError(messages.ErrorPluginPackageTooLarge, "large", map[string]any{"member": "bin/x", "max": 1}), 413},
		{messages.NewProductError(messages.ErrorPluginCertRevoked, "revoked", map[string]any{"publisher": "acme"}), 422},
		{messages.NewProductError(messages.ErrorPluginHostSetupUnsupported, "unsupported", map[string]any{"id": "acme_key"}), 422},
		{errors.New("disk on fire"), 500},
	} {
		rig := newPluginRig(t, &fakePlugins{err: tc.err})
		rec := rig.do(rig.session, "POST", "/api/v1/plugins/install", "pkg")
		if rec.Code != tc.status {
			t.Fatalf("%v: %d %s, want %d", tc.err, rec.Code, rec.Body, tc.status)
		}
		var pe *messages.ProductError
		if errors.As(tc.err, &pe) {
			var body struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != pe.Code() || body.Params["id"] != pe.Params()["id"] {
				t.Fatalf("%v: body %s", tc.err, rec.Body)
			}
		}
		if rec := rig.do(rig.session, "POST", "/api/v1/plugins/acme_backup/enable", ""); rec.Code != tc.status {
			t.Fatalf("enable with %v: %d", tc.err, rec.Code)
		}
	}
}

// A package whose id a compiled module has is a conflict, with the id in
// the literal body.
func TestPluginInstallRefusesACompiledModulesID(t *testing.T) {
	rig := newPluginRig(t, &fakePlugins{err: messages.NewProductError(messages.ErrorPluginIDTaken, "plugin id acme_backup is already used by a built-in module", map[string]any{"id": "acme_backup"})})
	rec := rig.do(rig.session, "POST", "/api/v1/plugins/install", "pkg")
	want := `{"code":"PLUGIN_ID_TAKEN","error":"plugin id acme_backup is already used by a built-in module","params":{"id":"acme_backup"}}`
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("install = %d %s", rec.Code, rec.Body)
	}
}

// Without a plugin host the routes still exist and say so; the assistant
// reads none of them.
func TestPluginRoutesWithoutAHostAndForTheAssistant(t *testing.T) {
	rig := newPluginRig(t, nil)
	for _, tc := range []struct{ method, path string }{{"GET", "/api/v1/plugins"}, {"POST", "/api/v1/plugins/install"}, {"DELETE", "/api/v1/plugins/x"}} {
		rec := rig.do(rig.session, tc.method, tc.path, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), messages.ErrorPluginsUnavailable) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	withHost := newPluginRig(t, &fakePlugins{})
	for _, path := range []string{"/api/v1/plugins", "/api/v1/metrics/plugins"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+withHost.session)
		req = req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true))
		rec := httptest.NewRecorder()
		withHost.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || len(withHost.plugins.recorded()) != 0 || strings.Contains(rec.Body.String(), "gpuworks_plugin") {
			t.Fatalf("assistant read of %s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// The metrics route answers the Prometheus text format, and a failure to
// write them is a 500 with nothing half-written.
func TestPluginMetricsRoute(t *testing.T) {
	rig := newPluginRig(t, &fakePlugins{})
	rec := rig.do(rig.session, "GET", "/api/v1/metrics/plugins", "")
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" ||
		rec.Body.String() != "# TYPE gpuworks_plugin_up gauge\ngpuworks_plugin_up{id=\"acme_backup\"} 1\n" {
		t.Fatalf("metrics: %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	rig = newPluginRig(t, &fakePlugins{metricsErr: errors.New("disk on fire")})
	if rec := rig.do(rig.session, "GET", "/api/v1/metrics/plugins", ""); rec.Code != 500 || strings.Contains(rec.Body.String(), "gpuworks_plugin_up") {
		t.Fatalf("failed metrics: %d %s", rec.Code, rec.Body)
	}
	rig = newPluginRig(t, nil)
	if rec := rig.do(rig.session, "GET", "/api/v1/metrics/plugins", ""); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), messages.ErrorPluginsUnavailable) {
		t.Fatalf("metrics without a plugin host: %d %s", rec.Code, rec.Body)
	}
}

// With nothing installed the list carries no check: it is what it was
// before plugins.
func TestPluginListWithNothingInstalledCarriesNoCheck(t *testing.T) {
	rig := newPluginRig(t, &fakePlugins{none: true})
	if rec := rig.do(rig.session, "GET", "/api/v1/plugins", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
}
