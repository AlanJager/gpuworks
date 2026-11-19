//go:build linux || darwin

package pluginbridge

import (
	"context"
	"io"
	"maps"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// gatedSpawner holds every spawn while blocking is set, until gate is
// closed, so a start stays in starting.
type gatedSpawner struct {
	flakySpawner
	blocking atomic.Bool
	gate     chan struct{}
}

func (g *gatedSpawner) Spawn(ctx context.Context, req pluginhost.SpawnRequest) (pluginhost.Process, error) {
	if g.blocking.Load() {
		<-g.gate
	}
	return g.flakySpawner.Spawn(ctx, req)
}

// A slow retry of a crashlooping plugin is not a recovery: while the retry
// is starting, the banner, its params and the failing check stay, and the
// console is not told anything changed. Only running again clears them.
func TestCrashloopBannerStaysWhileTheRetryStarts(t *testing.T) {
	w := newWorld(t, nil)
	sp := &gatedSpawner{gate: make(chan struct{})}
	sp.handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(sp.gate) }) })
	sp.failing.Store(true)
	timing := callTiming()
	timing.CrashloopCount, timing.CrashloopWindow, timing.CrashloopRetry = 3, time.Minute, 200*time.Millisecond
	var changed atomic.Int32
	b := w.openHealth(sp, timing, &changed)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)

	raised := waitBanners(t, b, "plugins.crashloop.acme_backup")
	announced := changed.Load()
	sp.failing.Store(false)
	sp.blocking.Store(true)
	waitState(t, b, "acme_backup", string(pluginhost.StateStarting))

	got := b.HealthBanners()
	if len(got) != 1 || got[0].ID != "plugins.crashloop.acme_backup" || got[0].Notice.Code() != messages.ErrorPluginCrashloop ||
		!maps.Equal(got[0].Notice.Params(), raised[0].Notice.Params()) {
		t.Fatalf("banners while the retry starts = %v", bannerIDs(got))
	}
	if c := b.Check(); c.Status != proto.CheckFail || c.Params["ids"] != "acme_backup" {
		t.Fatalf("check while the retry starts = %+v", c)
	}
	if changed.Load() != announced {
		t.Fatal("starting a retry was announced as a health change")
	}

	release.Do(func() { close(sp.gate) })
	waitState(t, b, "acme_backup", string(pluginhost.StateRunning))
	waitBanners(t, b)
	if c := b.Check(); c.Status != proto.CheckPass {
		t.Fatalf("check after recovery = %+v", c)
	}
}

// A package installed on a server that offered what it requires, opened by
// one that does not, is refused PLUGIN_INCOMPATIBLE: it raises the
// incompatible banner with the refusal's params and warns the check.
func TestAServerOfferingLessRaisesTheIncompatibleBanner(t *testing.T) {
	w := newWorld(t, nil)
	b, _ := w.openRunning()
	install(t, b, w.pkg("acme_backup", "1.0.0", func(s *extsign.Spec) {
		s.Manifest.Requires = map[string]int{"table.banners": 1}
	}), true)
	if ids := bannerIDs(b.HealthBanners()); len(ids) != 0 {
		t.Fatalf("banners %v while compatible", ids)
	}

	w.offered = extpkg.Capabilities()
	delete(w.offered, "table.banners")
	next, sp := w.openRunning()
	if v := next.view("acme_backup"); v.State != StateBroken || v.Code != messages.ErrorPluginIncompatible {
		t.Fatalf("view on a server offering less = %s %s", v.State, v.Code)
	}
	got := waitBanners(t, next, "plugins.incompatible.acme_backup")
	if p := got[0].Notice.Params(); got[0].Severity != extapi.SeverityWarning || got[0].Notice.Code() != messages.ErrorPluginIncompatible ||
		p["id"] != "acme_backup" || p["capability"] != "table.banners" || p["version"] != 1 || p["offered"] != 0 {
		t.Fatalf("incompatible banner = %+v %v", got[0], p)
	}
	if c := next.Check(); c.Status != proto.CheckWarn || c.Code != messages.CheckPluginsIncompatible || c.Params["ids"] != "acme_backup" {
		t.Fatalf("check of an incompatible package = %+v", c)
	}
	if len(sp.started()) != 0 {
		t.Fatal("an incompatible package was started")
	}
}
