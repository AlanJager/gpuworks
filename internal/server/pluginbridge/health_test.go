//go:build linux || darwin

package pluginbridge

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/extpkg/trust"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// flakySpawner fails every spawn while failing is set and otherwise runs
// the plugin over the real SDK.
type flakySpawner struct {
	sdkSpawner
	failing atomic.Bool
}

func (f *flakySpawner) Spawn(ctx context.Context, req pluginhost.SpawnRequest) (pluginhost.Process, error) {
	if f.failing.Load() {
		return nil, errors.New("exec format error")
	}
	return f.sdkSpawner.Spawn(ctx, req)
}

// openHealth opens and runs a bridge over sp that counts its health
// announcements in changed.
func (w *world) openHealth(sp pluginhost.Spawner, timing pluginhost.Timing, changed *atomic.Int32) *Bridge {
	w.t.Helper()
	pkgs, err := extpkg.NewHost(extpkg.Setup{
		DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_backup", "acme_driver"}},
	})
	if err != nil {
		w.t.Fatal(err)
	}
	b, err := Open(context.Background(), Config{Packages: pkgs, State: pushstate.Config{Store: w.st},
		Features: featuregate.NewDynamicSet(nil), Host: pluginhost.Config{Spawner: sp, Timing: timing},
		Core:          extapi.CoreInfo{Version: "dev", InstallID: "inst_bridge_test", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle},
		HealthChanged: func() { changed.Add(1) },
		Log:           slog.New(slog.NewTextHandler(&w.logs, nil))})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	w.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			w.t.Errorf("run: %v", err)
		}
	})
	return b
}

func bannerIDs(list []pushstate.Banner) []string {
	out := make([]string, len(list))
	for i, b := range list {
		out[i] = b.ID
	}
	return out
}

func waitBanners(t *testing.T, b *Bridge, want ...string) []pushstate.Banner {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := b.HealthBanners()
		if slices.Equal(bannerIDs(got), want) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("health banners %v, want %v", bannerIDs(got), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A crashlooping plugin raises PLUGIN_CRASHLOOP with its status params and
// fails the plugins check; once it runs again the banner is gone, the check
// passes, and the console was told both times. Disabling a crashlooping
// package clears its banner too.
func TestCrashloopRaisesTheBannerAndRecoveryClearsIt(t *testing.T) {
	w := newWorld(t, nil)
	sp := &flakySpawner{sdkSpawner: sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}}
	sp.failing.Store(true)
	timing := callTiming()
	timing.CrashloopCount, timing.CrashloopWindow, timing.CrashloopRetry = 3, time.Minute, 100*time.Millisecond
	var changed atomic.Int32
	b := w.openHealth(sp, timing, &changed)
	if len(b.HealthBanners()) != 0 || b.Check().Status != proto.CheckPass || changed.Load() != 0 {
		t.Fatalf("banners %v check %+v announced %d with nothing installed", b.HealthBanners(), b.Check(), changed.Load())
	}
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)

	got := waitBanners(t, b, "plugins.crashloop.acme_backup")
	n := got[0].Notice
	if got[0].Severity != extapi.SeverityDanger || n.Code() != messages.ErrorPluginCrashloop || n.Params()["id"] != "acme_backup" ||
		n.Params()["crashes"] != 3 || n.Params()["retry_s"] != 0 {
		t.Fatalf("crashloop banner = %+v %v", got[0], n.Params())
	}
	if c := b.Check(); c.ID != CheckID || c.Status != proto.CheckFail || c.Code != messages.CheckPluginsFailing || c.Params["ids"] != "acme_backup" {
		t.Fatalf("check while crashlooping = %+v", c)
	}
	raised := changed.Load()
	if raised == 0 {
		t.Fatal("raising the banner was not announced")
	}

	sp.failing.Store(false)
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	waitBanners(t, b)
	if c := b.Check(); c.Status != proto.CheckPass || c.Code != messages.CheckPluginsOK {
		t.Fatalf("check after recovery = %+v", c)
	}
	if changed.Load() == raised {
		t.Fatal("clearing the banner was not announced")
	}

	sp.failing.Store(true)
	if _, err := b.Disable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Enable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitBanners(t, b, "plugins.crashloop.acme_backup")
	if _, err := b.Disable(context.Background(), "acme_backup", "admin"); err != nil {
		t.Fatal(err)
	}
	waitBanners(t, b)
	if c := b.Check(); c.Status != proto.CheckPass {
		t.Fatalf("check with the crashlooping package disabled = %+v", c)
	}
}

// Revoking the certificate an enabled package was signed with makes it
// revoked at the next boot: it does not start, raises PLUGIN_REVOKED with
// the params of its list row (id, version, replacement) and fails the
// plugins check.
func TestRevokingTheKeyRaisesTheRevokedBanner(t *testing.T) {
	w := newWorld(t, nil)
	b, _ := w.openRunning()
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	if ids := bannerIDs(b.HealthBanners()); len(ids) != 0 {
		t.Fatalf("banners %v before the revocation", ids)
	}
	crl := extsign.CRL(t, 1, "acme", []extapi.Revocation{{Publisher: "acme", CertSerial: 1}}, w.pub.Key)
	if _, err := trust.Import(w.st, b.pkgs.Verifier.Trust, crl, w.pub.CertBundle(t)); err != nil {
		t.Fatal(err)
	}

	next, sp := w.openRunning() // the next boot reads the revocation
	if v := next.view("acme_backup"); v.State != StateRevoked {
		t.Fatalf("state after the revocation = %s (%s)", v.State, v.Code)
	}
	got := waitBanners(t, next, "plugins.revoked.acme_backup")
	if n := got[0].Notice; got[0].Severity != extapi.SeverityDanger || n.Code() != messages.ErrorPluginRevoked || n.Params()["id"] != "acme_backup" ||
		n.Params()["version"] != "1.0.0" || n.Params()["replacement"] != "" {
		t.Fatalf("revoked banner = %+v %v", got[0], n.Params())
	}
	if c := next.Check(); c.Status != proto.CheckFail || c.Params["ids"] != "acme_backup" {
		t.Fatalf("check of a revoked package = %+v", c)
	}
	if len(sp.started()) != 0 {
		t.Fatal("a revoked package was started")
	}
	if err := next.Uninstall(context.Background(), "acme_backup", false, "admin"); err != nil {
		t.Fatal(err)
	}
	waitBanners(t, next)
}

func view(id, state string, enabled bool, code string, params map[string]any) View {
	return View{ID: id, State: state, Enabled: enabled, Code: code, Params: params, Message: "m"}
}

// Each check status, with the ids it names: failing packages (crashloop,
// failed, broken, revoked) fail, incompatible ones warn, and disabled,
// running, idle or backing-off ones pass. Banners come only for crashloop,
// revoked and incompatible, danger first.
func TestCheckAndBannersOfEachHealth(t *testing.T) {
	incompatible := map[string]any{"id": "acme_old", "capability": "node.app", "version": 1, "offered": 0, "min_core_hint": "v27.3"}
	crash := map[string]any{"id": "acme_loop", "crashes": 5, "window_s": 600, "retry_s": 300}
	for _, c := range []struct {
		name    string
		views   []View
		status  string
		code    string
		ids     any
		banners []string
	}{
		{"nothing", nil, proto.CheckPass, messages.CheckPluginsOK, nil, nil},
		{"healthy", []View{
			view("acme_a", string(pluginhost.StateRunning), true, "", nil),
			view("acme_b", StateIdle, true, "", nil),
			view("acme_c", string(pluginhost.StateBackoff), true, messages.ErrorPluginCrashed, map[string]any{"id": "acme_c"}),
			view("acme_d", StateDisabled, false, "", nil),
			view("acme_e", StateInstalled, true, "", nil),
		}, proto.CheckPass, messages.CheckPluginsOK, nil, nil},
		{"disabled ones never count", []View{
			view("acme_loop", string(pluginhost.StateCrashloop), false, messages.ErrorPluginCrashloop, crash),
			view("acme_rev", StateRevoked, false, messages.ErrorPluginPackageRevoked, nil),
			view("acme_old", StateBroken, false, messages.ErrorPluginIncompatible, incompatible),
		}, proto.CheckPass, messages.CheckPluginsOK, nil, nil},
		{"incompatible warns", []View{
			view("acme_old", StateBroken, true, messages.ErrorPluginIncompatible, incompatible),
			view("acme_a", string(pluginhost.StateRunning), true, "", nil),
		}, proto.CheckWarn, messages.CheckPluginsIncompatible, "acme_old", []string{"plugins.incompatible.acme_old"}},
		{"failing wins over incompatible", []View{
			view("acme_broken", StateBroken, true, messages.ErrorPluginFileMismatch, nil),
			view("acme_failed", string(pluginhost.StateFailed), true, extapi.ErrorStateTooNew, nil),
			view("acme_loop", string(pluginhost.StateCrashloop), true, messages.ErrorPluginCrashloop, crash),
			view("acme_old", StateBroken, true, messages.ErrorPluginIncompatible, incompatible),
			view("acme_rev", StateRevoked, true, messages.ErrorPluginPackageRevoked, nil),
		}, proto.CheckFail, messages.CheckPluginsFailing, "acme_broken, acme_failed, acme_loop, acme_rev",
			[]string{"plugins.crashloop.acme_loop", "plugins.revoked.acme_rev", "plugins.incompatible.acme_old"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := checkOf(c.views)
			if got.ID != CheckID || got.Status != c.status || got.Code != c.code || got.Params["ids"] != c.ids || got.Message == "" {
				t.Fatalf("check = %+v, want %s %s ids %v", got, c.status, c.code, c.ids)
			}
			list := bannersOf(c.views)
			if ids := bannerIDs(list); !slices.Equal(ids, c.banners) {
				t.Fatalf("banners = %v, want %v", ids, c.banners)
			}
			for _, b := range list {
				if strings.HasPrefix(b.ID, "plugins.incompatible.") {
					if b.Severity != extapi.SeverityWarning || b.Notice.Code() != messages.ErrorPluginIncompatible || b.Notice.Params()["id"] != "acme_old" ||
						b.Notice.Params()["capability"] != "node.app" {
						t.Fatalf("incompatible banner = %+v %v", b, b.Notice.Params())
					}
				}
			}
		})
	}
}

// The bridge's metrics cover every installed package, labelled by its
// package state, and nothing after it is uninstalled; a call is counted.
func TestBridgeMetricsFollowTheInstalledPackages(t *testing.T) {
	w := newWorld(t, nil)
	sp := &sdkSpawner{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })}
	var changed atomic.Int32
	b := w.openHealth(sp, callTiming(), &changed)
	install(t, b, w.pkg("acme_driver", "1.0.0", onDemand), true)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	waitState(t, b, "acme_driver", StateIdle)
	resp, err := get(t, b, "acme_driver")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var buf bytes.Buffer
	if err := b.WriteMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`gpuworks_plugin_state{id="acme_backup",state="disabled"} 1`,
		`gpuworks_plugin_up{id="acme_backup"} 0`,
		`gpuworks_plugin_calls_total{id="acme_driver",class="member",outcome="ok"} 1`,
	} {
		if !strings.Contains(out, want+"\n") {
			t.Fatalf("metrics lack %q:\n%s", want, out)
		}
	}
	if err := b.Uninstall(context.Background(), "acme_driver", false, "admin"); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := b.WriteMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "acme_driver") {
		t.Fatalf("an uninstalled package is still exposed:\n%s", buf.String())
	}
	if _, kept := b.host.Status("acme_driver"); kept {
		// The host keeps the stopped status; its counters are gone.
		buf.Reset()
		_ = b.host.WriteMetrics(&buf, []pluginhost.MetricsRow{{ID: "acme_driver", State: "stopped"}})
		if strings.Contains(buf.String(), `outcome="ok"} 1`) {
			t.Fatal("the uninstalled package's counters were kept")
		}
	}
}
