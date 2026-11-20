//go:build linux || darwin

package pluginbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// Restart starts a crashlooping plugin as a new instance: it runs at once,
// without waiting for the slow retry, and its restarts count from zero; it
// is offered to an enabled package with a server part and refused for a
// disabled one (PLUGIN_DISABLED) or one that is not installed.
func TestRestartStartsANewInstance(t *testing.T) {
	w := newWorld(t, nil)
	sp := &flakySpawner{sdkSpawner: sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}}
	sp.failing.Store(true)
	timing := callTiming()
	timing.CrashloopCount, timing.CrashloopWindow, timing.CrashloopRetry = 3, time.Minute, time.Hour
	var changed atomic.Int32
	b := w.openHealth(sp, timing, &changed)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	v := waitState(t, b, "acme_backup", string(pluginhost.StateCrashloop))
	if v.Restarts == 0 || !slices.Equal(v.Actions, []string{ActionDisable, ActionRestart, ActionUninstall, ActionPurge, ActionUpgrade}) {
		t.Fatalf("crashlooping view = %+v", v)
	}
	before, _ := b.host.Logs("acme_backup", pluginhost.LogCursor{})

	sp.failing.Store(false)
	v, err := b.Restart(context.Background(), "acme_backup", "admin")
	if err != nil {
		t.Fatal(err)
	}
	v = waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	if v.Restarts != 0 {
		t.Fatalf("restarts after a restart = %d", v.Restarts)
	}
	waitBanners(t, b)
	after, _ := b.host.Logs("acme_backup", before.Next)
	states := make([]pluginhost.State, len(after.Events))
	for i, e := range after.Events {
		states[i] = e.State
	}
	if !slices.Contains(states, pluginhost.StateStopped) || states[len(states)-1] != pluginhost.StateRunning {
		t.Fatalf("events kept across the restart = %v", states)
	}

	if _, err := b.Disable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Restart(context.Background(), "acme_backup", "admin"); !isCode(err, messages.ErrorPluginDisabled) {
		t.Fatalf("restart of a disabled package = %v", err)
	}
	if _, err := b.Restart(context.Background(), "acme_nothing", "admin"); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("restart of nothing = %v", err)
	}
}

// Restarting an on-demand plugin stops it and leaves it idle until its
// next call.
func TestRestartOfAnOnDemandPluginGoesIdle(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	timing := callTiming()
	timing.IdleStop = time.Hour
	var changed atomic.Int32
	b := w.openHealth(sp, timing, &changed)
	install(t, b, w.pkg("acme_backup", "1.0.0", onDemand), true)
	v := waitState(t, b, "acme_backup", string(pluginhost.StateIdle))
	if !v.OnDemand || !slices.Contains(v.Actions, ActionRestart) {
		t.Fatalf("idle view = %+v", v)
	}
	resp, err := get(t, b, "acme_backup")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	if _, err := b.Restart(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "acme_backup", string(pluginhost.StateIdle))
	if sp.count() != 1 {
		t.Fatalf("a restart of an on-demand plugin spawned %d processes", sp.count())
	}
}

// An uninstall runs only when it is confirmed with the digest of its
// impact now: without one, or with the digest of an impact a push has
// changed since, it is refused with the counts and removes nothing.
func TestUninstallNeedsTheDigestOfItsImpact(t *testing.T) {
	w := newWorld(t, nil)
	b := w.open()
	install(t, b, w.pkg("acme_backup", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:node_holds"}
		s.Manifest.Uses = map[string]int{"table.node_holds": 1}
	}), false)
	pushHolds(t, b, w, "acme_backup", 1, `{"nodes":{"3":{"code":"FEATURE_UNAVAILABLE"}}}`)

	im, err := b.Uninstall(context.Background(), "acme_backup", UninstallRequest{By: "admin"})
	if !isCode(err, messages.ErrorPluginUninstallConfirmRequired) || !slices.Equal(im.Nodes, []uint{3}) || im.Version != "1.0.0" {
		t.Fatalf("unconfirmed uninstall = %+v, %v", im, err)
	}
	params := err.(*messages.ProductError).Params()
	if params["id"] != "acme_backup" || params["confirm"] != string(im.Digest) || params["held_nodes"] != 1 || params["caps_lifted"] != 0 ||
		params["features_locked"] != 0 || params["users_affected"] != 0 || params["banners"] != 0 || params["overrides"] != 0 {
		t.Fatalf("params = %v", params)
	}
	preview := im.Digest
	pushHolds(t, b, w, "acme_backup", 2, `{"nodes":{"3":{"code":"FEATURE_UNAVAILABLE"},"4":{"code":"FEATURE_UNAVAILABLE"}}}`)
	if im, err := b.Uninstall(context.Background(), "acme_backup", UninstallRequest{Confirm: preview, By: "admin"}); !isCode(err, messages.ErrorPluginUninstallConfirmRequired) ||
		im.Digest == preview || !slices.Equal(im.Nodes, []uint{3, 4}) {
		t.Fatalf("uninstall confirmed with a stale digest = %+v, %v", im, err)
	}
	if len(b.List()) != 1 || len(b.state.Held("acme_backup")) != 1 {
		t.Fatal("a refused uninstall removed something")
	}
	if err := uninstallConfirmed(t, b, context.Background(), "acme_backup", false); err != nil {
		t.Fatal(err)
	}
	if len(b.List()) != 0 || len(b.state.Held("acme_backup")) != 0 {
		t.Fatal("the confirmed uninstall left the package or its rows")
	}
	// Its directory, with its data, stays for a purge, whose impact has
	// nothing left to release.
	if im, err := b.Impact("acme_backup"); err != nil || len(im.Tables)+len(im.Nodes) != 0 || im.Version != "" {
		t.Fatalf("impact after the uninstall = %+v, %v", im, err)
	}
	if err := uninstallConfirmed(t, b, context.Background(), "acme_backup", true); err != nil {
		t.Fatalf("purge after the uninstall = %v", err)
	}
	if _, err := b.Impact("acme_backup"); !isCode(err, messages.ErrorPluginNotInstalled) {
		t.Fatalf("impact after the purge = %v", err)
	}
}

// The report of a package reads its tables and down period; its logs
// follow its supervision; an id that is neither installed nor held is not
// installed.
func TestReportAndLogsOfAPackage(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	var changed atomic.Int32
	b := w.openHealth(sp, callTiming(), &changed)
	install(t, b, w.pkg("acme_backup", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:node_holds"}
		s.Manifest.Uses = map[string]int{"table.node_holds": 1}
	}), false)
	// Pushed as no process yet: a later push must come over its process's
	// connection.
	pushHolds(t, b, w, "acme_backup", 1, `{"nodes":{"3":{"code":"FEATURE_UNAVAILABLE"}}}`)
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	logs, follow, err := b.Logs("acme_backup", pluginhost.LogCursor{})
	if err != nil || len(logs.Events) < 2 || logs.Events[len(logs.Events)-1].State != pluginhost.StateRunning || follow == nil {
		t.Fatalf("logs = %+v, %v", logs, err)
	}
	if _, err := b.Disable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-follow:
	case <-time.After(5 * time.Second):
		t.Fatal("the disable woke no follower")
	}
	r, err := b.Report("acme_backup")
	if err != nil || !r.Installed || len(r.Tables) != 1 || r.Tables[0].Kind != extapi.KindNodeHolds || r.DownSince.IsZero() {
		t.Fatalf("report = %+v, %v", r, err)
	}
	for _, call := range []func() error{
		func() error { _, err := b.Report("acme_nothing"); return err },
		func() error { _, _, err := b.Logs("acme_nothing", pluginhost.LogCursor{}); return err },
		func() error { _, err := b.Impact("acme_nothing"); return err },
	} {
		if err := call(); !isCode(err, messages.ErrorPluginNotInstalled) {
			t.Fatalf("report of nothing = %v", err)
		}
	}
}

// Every state a view can take offers exactly its actions: removal and
// upgrade always, rollback with a previous version, enable for a disabled
// or host-setup package, disable once enabled, and restart only for an
// enabled server part that may run.
func TestViewActionsPerState(t *testing.T) {
	b := newWorld(t, nil).open()
	server := func(id string) extpkg.Entry {
		e := entryWith(id, "1.0.0", true)
		e.Verified.Manifest.Runtime = &extapi.RuntimeDecl{}
		e.Verified.Binary = "bin/" + id
		return e
	}
	revoked := entryWith("acme_revoked", "1.0.0", true)
	revoked.Verified.Revoked = &extapi.Revocation{}
	b.mu.Lock()
	b.refusals["acme_failed"] = messages.NewProductError(messages.ErrorPluginBinaryMissing, "no binary", map[string]any{"id": "acme_failed"})
	b.refusals["acme_setup"] = messages.NewProductError(messages.ErrorPluginHostSetupRequired, "host setup", map[string]any{"id": "acme_setup"})
	b.mu.Unlock()
	removal := []string{ActionUninstall, ActionPurge, ActionUpgrade}
	rollback := entryWith("acme_old", "2.0.0", false)
	rollback.Versions = []string{"1.0.0", "2.0.0"}
	for _, c := range []struct {
		entry   extpkg.Entry
		state   string
		actions []string
	}{
		{extpkg.Entry{ID: "acme_broken", Version: "1.0.0", Err: errors.New("a file changed")}, StateBroken, removal},
		{extpkg.Entry{ID: "acme_unverified", Version: "1.0.0"}, StateBroken, removal},
		{revoked, StateRevoked, removal},
		{entryWith("acme_off", "1.0.0", false), StateDisabled, append([]string{ActionEnable}, removal...)},
		{entryWith("acme_ui", "1.0.0", true), StateInstalled, append([]string{ActionDisable}, removal...)},
		{server("acme_failed"), string(pluginhost.StateFailed), append([]string{ActionDisable, ActionRestart}, removal...)},
		{server("acme_setup"), StateNeedsHostSetup, append([]string{ActionEnable, ActionDisable}, removal...)},
		{server("acme_new"), string(pluginhost.StateStarting), append([]string{ActionDisable, ActionRestart}, removal...)},
		{rollback, StateDisabled, append(append([]string{ActionEnable}, removal...), ActionRollback)},
	} {
		v := b.viewOf(c.entry)
		if v.State != c.state || !slices.Equal(v.Actions, c.actions) {
			t.Errorf("%s: state %q actions %v, want %q %v", c.entry.ID, v.State, v.Actions, c.state, c.actions)
		}
	}
}
