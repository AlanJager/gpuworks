package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func fakeImpact(id string) dto.PluginImpact {
	return dto.PluginImpact{ID: id, Version: "1.0.0", Confirm: "sha256:aa", Tables: []dto.PluginImpactTable{{Kind: "node_holds", Entries: 1}},
		Nodes: []uint{3}, Features: []string{"acme.backup"}, Caps: []string{"max_cards"}, Overrides: []int64{}}
}

func (f *fakePlugins) Restart(_ context.Context, id, by string) (dto.Plugin, error) {
	return dto.Plugin{ID: id, State: "starting"}, f.record("restart " + id + " " + by)
}

func (f *fakePlugins) Impact(_ context.Context, id string) (dto.PluginImpact, error) {
	return fakeImpact(id), f.record("impact " + id)
}

func (f *fakePlugins) State(_ context.Context, id string) (dto.PluginState, error) {
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	down, clamp, next := now.Add(-time.Minute), now.Add(time.Hour), now.Add(24*time.Hour)
	return dto.PluginState{ID: id, Installed: true, Verified: true, Now: now, DownSince: &down, GraceS: 3600, ClampAt: &clamp,
		Tables:    []dto.PluginStateTable{{Kind: "availability", Schema: 1, Generation: 4, Digest: "sha256:bb", Entries: 2, PushedAt: down, NextStep: &next}},
		Overrides: []dto.PluginOverride{}}, f.record("state " + id)
}

func (f *fakePlugins) Catalog(context.Context) (dto.PluginCatalog, error) {
	return dto.PluginCatalog{Items: []dto.PluginCatalogEntry{{ID: "acme_backup", Publisher: "acme", Installed: "1.0.0", BestVersion: "1.1.0", MinCoreHint: "v27.3",
		Installable: true, Refused: []dto.PluginCatalogRefusal{{Version: "2.0.0", Message: dto.ProductMessage{Code: messages.ErrorPluginNeedsNewerCore,
			Params: map[string]any{"id": "acme_backup"}, Message: "needs a newer server"}}}}}, Errors: []dto.PluginCatalogError{}}, f.record("catalog")
}

func (f *fakePlugins) Logs(_ context.Context, id string, from PluginLogCursor) (PluginLogs, <-chan struct{}, error) {
	f.mu.Lock()
	f.logReads++
	n := f.logReads
	f.mu.Unlock()
	if err := f.record("logs " + id); err != nil {
		return PluginLogs{}, nil, err
	}
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if n == 1 {
		return PluginLogs{Events: []dto.PluginEvent{{Seq: 7, At: at, State: "starting"}, {Seq: 8, At: at, State: "running"}}, EventsTruncated: true,
			Stderr: []byte("hello\n"), StderrFrom: 10, StderrTruncated: true, Next: PluginLogCursor{Event: 8, Stderr: 16}}, f.follow, nil
	}
	if from != (PluginLogCursor{Event: 8, Stderr: 16}) {
		return PluginLogs{}, nil, messages.NewProductError(messages.ErrorPluginNotInstalled, "bad cursor", map[string]any{"id": id})
	}
	return PluginLogs{Events: []dto.PluginEvent{{Seq: 9, At: at, State: "backoff", Message: &dto.ProductMessage{Code: messages.ErrorPluginCrashed, Message: "plugin acme_backup is backoff"}}},
		Next: PluginLogCursor{Event: 9, Stderr: 16}}, nil, nil
}

// The reports, the restart and the refused uninstall answer their literal
// wire shapes.
func TestPluginReportRoutesWire(t *testing.T) {
	plugins := &fakePlugins{}
	rig := newPluginRig(t, plugins)
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/api/v1/plugins/acme_backup/impact", `{"id":"acme_backup","version":"1.0.0","confirm":"sha256:aa","tables":[{"kind":"node_holds","entries":1}],` +
			`"nodes":[3],"features":["acme.backup"],"caps":["max_cards"],"banners":0,"overrides":[],"users_affected":0}`},
		{"GET", "/api/v1/plugins/acme_backup/state", `{"id":"acme_backup","installed":true,"verified":true,"now":"2026-12-01T00:00:00Z",` +
			`"down_since":"2026-11-30T23:59:00Z","grace_s":3600,"clamp_at":"2026-12-01T01:00:00Z","tables":[{"kind":"availability","schema":1,` +
			`"generation":4,"digest":"sha256:bb","entries":2,"pushed_at":"2026-11-30T23:59:00Z","next_step":"2026-12-02T00:00:00Z"}],"overrides":[]}`},
		{"GET", "/api/v1/plugins/catalog", `{"items":[{"id":"acme_backup","publisher":"acme","installed":"1.0.0","best_version":"1.1.0",` +
			`"min_core_hint":"v27.3","installable":true,"refused":[{"version":"2.0.0","message":{"code":"PLUGIN_NEEDS_NEWER_CORE",` +
			`"params":{"id":"acme_backup"},"message":"needs a newer server"}}]}],"errors":[]}`},
		{"POST", "/api/v1/plugins/acme_backup/restart", `{"id":"acme_backup","version":"","enabled":false,"state":"starting","restarts":0,` +
			`"parts":null,"isolation":"","roles":null,"permissions":null,"on_demand":false,"features":null,"actions":null}`},
		{"DELETE", "/api/v1/plugins/acme_backup?confirm=sha256:old", `{"code":"PLUGIN_UNINSTALL_CONFIRM_REQUIRED","error":"confirm",` +
			`"impact":{"id":"acme_backup","version":"1.0.0","confirm":"sha256:aa","tables":[{"kind":"node_holds","entries":1}],"nodes":[3],` +
			`"features":["acme.backup"],"caps":["max_cards"],"banners":0,"overrides":[],"users_affected":0},` +
			`"params":{"banners":0,"caps_lifted":1,"confirm":"sha256:aa","features_locked":1,"held_nodes":1,"id":"acme_backup","overrides":0,"users_affected":0}}`},
	} {
		rec := rig.do(rig.session, tc.method, tc.path, "")
		status := 200
		if tc.method == "DELETE" {
			status = http.StatusConflict
		}
		if rec.Code != status || strings.TrimSpace(rec.Body.String()) != tc.want {
			t.Errorf("%s %s: %d\n%s\nwant\n%s", tc.method, tc.path, rec.Code, rec.Body, tc.want)
		}
	}
	got := strings.Join(plugins.recorded(), "|")
	if got != "impact acme_backup|state acme_backup|catalog|restart acme_backup admin|uninstall acme_backup keep sha256:old admin" {
		t.Fatalf("calls = %s", got)
	}
}

// Refusals keep their codes: a disabled package's restart and an
// unconfirmed uninstall are conflicts, an unknown package is not found.
func TestPluginReportRoutesMapRefusals(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{messages.NewProductError(messages.ErrorPluginDisabled, "disabled", map[string]any{"id": "acme_backup"}), 409},
		{messages.NewProductError(messages.ErrorPluginNotInstalled, "no", map[string]any{"id": "acme_backup"}), 404},
	} {
		rig := newPluginRig(t, &fakePlugins{err: tc.err})
		for _, r := range []struct{ method, path string }{{"POST", "/api/v1/plugins/acme_backup/restart"}, {"GET", "/api/v1/plugins/acme_backup/state"},
			{"GET", "/api/v1/plugins/acme_backup/impact"}, {"GET", "/api/v1/plugins/catalog"}, {"GET", "/api/v1/plugins/acme_backup/logs"}} {
			if rec := rig.do(rig.session, r.method, r.path, ""); rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"code":"`) {
				t.Errorf("%s %s with %v: %d %s", r.method, r.path, tc.err, rec.Code, rec.Body)
			}
		}
	}
	if pluginErrorStatus(messages.NewProductError(messages.ErrorPluginUninstallConfirmRequired, "x", nil)) != http.StatusConflict {
		t.Fatal("an unconfirmed uninstall is not a conflict")
	}
}

// Without a plugin host the new routes say so; the assistant reads none of
// them.
func TestPluginReportRoutesWithoutAHostAndForTheAssistant(t *testing.T) {
	routes := []struct{ method, path string }{{"GET", "/api/v1/plugins/catalog"}, {"GET", "/api/v1/plugins/x/state"}, {"GET", "/api/v1/plugins/x/impact"},
		{"GET", "/api/v1/plugins/x/logs"}, {"POST", "/api/v1/plugins/x/restart"}}
	rig := newPluginRig(t, nil)
	for _, tc := range routes {
		if rec := rig.do(rig.session, tc.method, tc.path, ""); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), messages.ErrorPluginsUnavailable) {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, rec.Code, rec.Body)
		}
	}
	withHost := newPluginRig(t, &fakePlugins{})
	for _, tc := range routes {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+withHost.session)
		req = req.WithContext(context.WithValue(req.Context(), assistantReadKey{}, true))
		rec := httptest.NewRecorder()
		withHost.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound || len(withHost.plugins.recorded()) != 0 {
			t.Fatalf("assistant read of %s: %d %s", tc.path, rec.Code, rec.Body)
		}
	}
}

// The logs stream sends the backlog (truncation marked on its first event
// and on the stderr frame), then a follow frame when the plugin logs more,
// read from the cursor the backlog ended at; a revoked login ends it.
func TestPluginLogsStreamBacklogThenFollows(t *testing.T) {
	old := streamAuthRecheck
	streamAuthRecheck = 20 * time.Millisecond
	t.Cleanup(func() { streamAuthRecheck = old })
	plugins := &fakePlugins{follow: make(chan struct{})}
	rig := newPluginRig(t, plugins)
	srv := httptest.NewServer(rig.h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/plugins/acme_backup/logs", nil)
	req.Header.Set("Authorization", "Bearer "+rig.session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("logs = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	frames := make(chan string, 16)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		var frame []string
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				frame = append(frame, line)
				continue
			}
			if len(frame) > 0 && !strings.HasPrefix(frame[0], ":") {
				frames <- strings.Join(frame, "\n")
			}
			frame = nil
		}
	}()
	next := func() string {
		select {
		case f := <-frames:
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("no frame")
			return ""
		}
	}
	for _, want := range []string{
		"event: state\ndata: {\"seq\":7,\"at\":\"2026-12-01T00:00:00Z\",\"state\":\"starting\",\"truncated\":true}",
		"event: state\ndata: {\"seq\":8,\"at\":\"2026-12-01T00:00:00Z\",\"state\":\"running\"}",
		"event: stderr\ndata: {\"offset\":10,\"text\":\"hello\\n\",\"truncated\":true}",
	} {
		if got := next(); got != want {
			t.Fatalf("backlog frame = %q\nwant %q", got, want)
		}
	}
	close(plugins.follow)
	if got, want := next(), "event: state\ndata: {\"seq\":9,\"at\":\"2026-12-01T00:00:00Z\",\"state\":\"backoff\",\"message\":{\"code\":\"PLUGIN_CRASHED\",\"message\":\"plugin acme_backup is backoff\"}}"; got != want {
		t.Fatalf("follow frame = %q\nwant %q", got, want)
	}
	// Revoked as another process would (reset-password): the stream's
	// recheck finds it.
	if err := rig.st.DB().Where("token_hash <> ''").Delete(&store.ManagementSession{}).Error; err != nil {
		t.Fatal(err)
	}
	select {
	case _, open := <-frames:
		if open {
			for range frames {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked login kept the stream open")
	}
}
