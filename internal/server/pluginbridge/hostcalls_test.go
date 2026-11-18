package pluginbridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
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
func openOnClock(t *testing.T, w *world, clk *settableClock, corrected func(from, to time.Time, by string)) (b *Bridge, stop func()) {
	t.Helper()
	pkgs, err := extpkg.NewHost(extpkg.Setup{DataDir: w.dir, Roots: []extapi.PublicKey{w.root.Public}, Store: w.st})
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
	return b, stop
}

// A clock authority's correction goes through pushed state's bounded path:
// beyond the bound it is refused with the core's code and params, within it
// it moves extclock, reports the correction, writes an audit line with the
// plugin's reason, and still holds after the server restarts.
func TestAPluginsClockCorrectionIsBoundedAuditedAndSurvivesARestart(t *testing.T) {
	w := newWorld(t, nil)
	ahead := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	right := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	clk := &settableClock{wall: ahead}
	var mu sync.Mutex
	var effects []string
	corrected := func(from, to time.Time, by string) {
		mu.Lock()
		defer mu.Unlock()
		effects = append(effects, from.Format(time.RFC3339)+">"+to.Format(time.RFC3339)+" by "+by)
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
	if len(got) != 1 || got[0] != "2027-03-01T00:00:00Z>2027-01-15T00:00:00Z by acme_quota" {
		t.Fatalf("effects = %v", got)
	}
	logs := w.logs.String()
	if !strings.Contains(logs, "audit=true") || !strings.Contains(logs, `reason="acme correction code 7F3A-22C1"`) || !strings.Contains(logs, "plugin=acme_quota") {
		t.Fatalf("no audit line with the reason:\n%s", logs)
	}
	stop()

	b, _ = openOnClock(t, w, clk, corrected)
	if _, ext, _ := (stateHost{b}).Clocks(); !ext.Equal(right) {
		t.Fatalf("after a restart extclock is %v, want the corrected %v", ext, right)
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
