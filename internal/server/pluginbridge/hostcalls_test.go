package pluginbridge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// settableClock is a wall clock a test moves, with a monotonic reading that
// follows it.
type settableClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

func (c *settableClock) clock() pushstate.Clock {
	return pushstate.Clock{
		Wall: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.wall },
		Mono: func() time.Duration { c.mu.Lock(); defer c.mu.Unlock(); return c.mono },
	}
}

func (c *settableClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := at.Sub(c.wall); d > 0 {
		c.mono += d
	}
	c.wall = at
}

// openOnClock opens and runs a bridge over w's store whose pushed state
// follows clk and reports corrections to corrected; stop ends its run.
func openOnClock(t *testing.T, w *world, clk *settableClock, corrected func(from, to time.Time, by, reason string)) (b *Bridge, stop func()) {
	t.Helper()
	// The refusing spawner starts no process; match the existing world's
	// named fixture policy while retaining the packages' signed grants.
	pkgs, err := extpkg.NewHost(extpkg.Setup{
		DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st,
		Config: serverconfig.Plugins{AllowUnsandboxed: []string{"acme_backup", "acme_off", "acme_driver"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err = Open(context.Background(), Config{Packages: pkgs,
		State: pushstate.Config{Store: w.st, Clock: clk.clock(), TickInterval: time.Millisecond,
			Effects: pushstate.Effects{ClockCorrected: corrected}},
		Host: pluginhost.Config{Spawner: &refusingSpawner{}},
		Core: extapi.CoreInfo{Version: "dev", InstallID: "inst_bridge_test", OS: "linux", Arch: "amd64", HARole: extapi.HARoleSingle},
		Log:  slog.New(slog.NewTextHandler(&w.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("run: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	waitBridgeRunning(t, b)
	return b, stop
}

// A clock authority's correction goes through pushed state's bounded path:
// beyond the bound it is refused with the core's code and params, within it
// it moves extclock and reports the correction with the plugin's reason,
// and it still holds after the server restarts.
func TestAPluginsClockCorrectionIsBoundedAuditedAndSurvivesARestart(t *testing.T) {
	w := newWorld(t, nil)
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: ahead}
	var mu sync.Mutex
	var effects []string
	corrected := func(from, to time.Time, by, reason string) {
		mu.Lock()
		defer mu.Unlock()
		effects = append(effects, from.Format(time.RFC3339)+">"+to.Format(time.RFC3339)+" by "+by+": "+reason)
	}
	b, stop := openOnClock(t, w, clk, corrected)
	// The wall clock was put right; the high-water mark keeps extclock
	// ahead until a correction.
	clk.set(right)
	h := stateHost{b}
	owner := pluginhost.Owner{ID: "acme_quota", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 2}
	ctx := context.Background()

	_, _, err := h.Correct(ctx, owner, right.AddDate(-2, 0, 0), "too far")
	var refusal *pluginhost.Error
	if !errors.As(err, &refusal) || refusal.Wire.Code != messages.ErrorPluginClockCorrectionTooLarge ||
		refusal.Status != 400 || refusal.Wire.Params["max_days"] != 366 {
		t.Fatalf("beyond the bound = %v", err)
	}
	if wall, ext, _ := h.Clocks(); !wall.Equal(right) || !ext.Equal(ahead) {
		t.Fatalf("a refused correction moved the clocks: wall %v ext %v", wall, ext)
	}

	from, to, err := h.Correct(ctx, owner, right, "acme correction code 7F3A-22C1")
	if err != nil || !from.Equal(ahead) || !to.Equal(right) {
		t.Fatalf("correction = %v > %v, %v", from, to, err)
	}
	if _, ext, hwm := h.Clocks(); !ext.Equal(right) || !hwm.Equal(right) {
		t.Fatalf("after the correction ext %v hwm %v", ext, hwm)
	}
	mu.Lock()
	got := append([]string(nil), effects...)
	mu.Unlock()
	// The effect is the one audit line of the correction, with the
	// plugin's reason; the bridge writes none of its own.
	if len(got) != 1 || got[0] != "2027-03-01T00:00:00Z>2027-01-15T00:00:00Z by acme_quota: acme correction code 7F3A-22C1" {
		t.Fatalf("effects = %v", got)
	}
	if logs := w.logs.String(); strings.Contains(logs, "audit=true") {
		t.Fatalf("the bridge wrote its own audit line:\n%s", logs)
	}
	stop()

	b, _ = openOnClock(t, w, clk, corrected)
	if _, ext, _ := (stateHost{b}).Clocks(); !ext.Equal(right) {
		t.Fatalf("after a restart extclock is %v, want the corrected %v", ext, right)
	}
}

// A correction to a time before the wall clock answers, reports and shows
// the extclock it took effect at, the wall clock, not the requested one.
func TestAPluginsCorrectionAnswersTheExtclockItTookEffectAt(t *testing.T) {
	w := newWorld(t, nil)
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: ahead}
	var effects []string
	b, _ := openOnClock(t, w, clk, func(from, to time.Time, by, _ string) {
		effects = append(effects, from.Format(time.RFC3339)+">"+to.Format(time.RFC3339))
	})
	clk.set(right)
	owner := pluginhost.Owner{ID: "acme_quota", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 1}
	from, to, err := stateHost{b}.Correct(context.Background(), owner, right.Add(-5*time.Minute), "an older code")
	if err != nil || !from.Equal(ahead) || !to.Equal(right) {
		t.Fatalf("correction = %v > %v, %v; want %v > %v", from, to, err, ahead, right)
	}
	if _, ext, _ := (stateHost{b}).Clocks(); !ext.Equal(right) {
		t.Fatalf("extclock = %v", ext)
	}
	if len(effects) != 1 || effects[0] != "2027-03-01T00:00:00Z>2027-01-15T00:00:00Z" {
		t.Fatalf("effects = %v", effects)
	}
	notice := b.state.Banners(true)
	if len(notice) != 1 || notice[0].Notice.Params()["to"] != "2027-01-15T00:00:00Z" {
		t.Fatalf("banners = %+v", notice)
	}
}

// A step that fired at a clock that ran ahead un-fires once a plugin
// corrects extclock back over the host path.
func TestAPluginsCorrectionUnfiresAStep(t *testing.T) {
	w := newWorld(t, nil)
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: ahead}
	b, _ := openOnClock(t, w, clk, nil)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), false)
	b.mu.Lock()
	b.pushers["acme_backup"] = identity{version: "1.0.0", pusher: pushstate.Pusher{ID: "acme_backup", Publisher: "acme",
		Grants: w.pub.Cert.Grants, Permissions: []string{"push:banners"}, Codes: []string{"ACME_EARLY", "ACME_LATE"}}}
	b.mu.Unlock()
	h := stateHost{b}
	owner := pluginhost.Owner{ID: "acme_backup", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 1}
	push, err := extapi.DecodeStatePush([]byte(`{"body":{"banners":[{"audience":"all","id":"term","steps":[` +
		`{"notice":{"code":"ACME_EARLY"},"severity":"info"},` +
		`{"at":"2027-02-01T00:00:00Z","notice":{"code":"ACME_LATE"},"severity":"warning"}]}]},"generation":1,"kind":"banners","schema":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Push(context.Background(), owner, http.MethodPut, push); err != nil {
		t.Fatal(err)
	}
	shown := func() string {
		for _, bn := range b.state.Banners(false) {
			if bn.Notice != nil {
				return bn.Notice.Code()
			}
		}
		return ""
	}
	if got := shown(); got != "ACME_LATE" {
		t.Fatalf("at the clock that ran ahead the banner shows %q", got)
	}
	clk.set(right)
	if got := shown(); got != "ACME_LATE" {
		t.Fatalf("the wall clock alone un-fired the step: %q", got)
	}
	corrector := pluginhost.Owner{ID: "acme_quota", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 1}
	if _, _, err := h.Correct(context.Background(), corrector, right, "verified code"); err != nil {
		t.Fatal(err)
	}
	if got := shown(); got != "ACME_EARLY" {
		t.Fatalf("after the correction the banner shows %q, want the earlier step", got)
	}
}

// Ready and own-state reads reach pushed state as the connection's package.
func TestReadyAndOwnRowsGoToTheConnectionsPackage(t *testing.T) {
	w := newWorld(t, nil)
	clk := &settableClock{wall: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}
	b, _ := openOnClock(t, w, clk, nil)
	h := stateHost{b}
	owner := pluginhost.Owner{ID: "acme_quota", Version: "1.0.0", Lifecycle: extapi.LifecycleResident, Generation: 2}
	h.Ready(owner)
	if b.state.Ready("acme_quota", 2) {
		t.Fatal("the host's ready did not reach pushed state for the connection's process")
	}
	held, err := h.OwnRows(owner, extapi.KindBanners)
	if err != nil || held.Held() || held.Kind != extapi.KindBanners {
		t.Fatalf("own rows with nothing pushed = %+v, %v", held, err)
	}
}
