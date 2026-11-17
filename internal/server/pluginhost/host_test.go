package pluginhost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pluginexec"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

func banners(id string) *extapi.BannersV1 {
	return &extapi.BannersV1{Banners: []extapi.Banner{{ID: id, Audience: "admin",
		Steps: []extapi.BannerStep{{Severity: "info", Notice: &extapi.Notice{Code: "ACME_NOTICE"}}}}}}
}

func productCode(t *testing.T, err error) string {
	t.Helper()
	var pe *messages.ProductError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a product error", err)
	}
	return pe.Code()
}

// A resident plugin is started, its handshake accepted, and its push is
// applied for the package bound to its connection; its routes answer and
// its session carries a fresh principal key.
func TestResidentPluginRunsAndPushes(t *testing.T) {
	state := &fakeState{held: []extapi.HeldState{{Kind: "node_holds", Schema: 1, Generation: 3, Digest: testDigest}}}
	var seenHeld atomic.Int32
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Init = func(_ context.Context, p *pluginsdk.Plugin) error {
			seenHeld.Store(int32(len(p.Request().Held)))
			return nil
		}
		c.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
			_, err := p.PushState(ctx, extapi.KindBanners, banners("hello"))
			return err
		}
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) })
		return c
	}}
	h, log := newTestHost(t, sp, state, fastTiming())
	spec := testSpec(t, "acme_backup")
	if err := h.Start(spec); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(spec); err == nil {
		t.Fatal("a plugin was supervised twice")
	}
	st := waitStatus(t, h, "acme_backup", inState(StateRunning))
	if st.Use["table.banners"] != 1 || st.PID != 1001 || st.Starts != 1 {
		t.Fatalf("status = %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(state.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	pushes := state.recorded()
	if len(pushes) != 1 || pushes[0].owner.ID != "acme_backup" || pushes[0].method != http.MethodPut ||
		pushes[0].push.Kind != "banners" || pushes[0].owner.Use["table.banners"] != 1 {
		t.Fatalf("pushes = %+v", pushes)
	}
	if seenHeld.Load() != 1 {
		t.Fatalf("the plugin saw %d held tables, want the state host's one", seenHeld.Load())
	}
	sess, ok := h.Session("acme_backup")
	if !ok || len(sess.PrincipalKey) != extapi.PrincipalKeyBytes || sess.Use["table.banners"] != 1 {
		t.Fatalf("session = %+v", sess)
	}
	req, _ := http.NewRequest(http.MethodPost, "/r/jobs", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := h.Do(ctx, "acme_backup", req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("route = %d", resp.StatusCode)
	}
	// The spawn carried the sandbox and the filtered environment.
	sp.mu.Lock()
	r := sp.reqs[0]
	sp.mu.Unlock()
	if r.Profile.Landlock == nil || !slices.Contains(r.Profile.Landlock.ReadWrite, spec.DataDir) || r.Profile.Nice != 5 ||
		!slices.Contains(r.Env, "GWX_SERVE_FD=3") || !slices.Contains(r.Env, "HOME="+spec.DataDir) {
		t.Fatalf("spawn request = %+v", r)
	}
	if err := h.Stop(context.Background(), "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if got := log.states("acme_backup"); !slices.Equal(got, []State{StateStarting, StateRunning, StateStopping, StateStopped}) {
		t.Fatalf("states = %v", got)
	}
	if sent := sp.proc(1).sent(); len(sent) != 0 {
		t.Fatalf("a plugin that obeyed shutdown was signalled: %v", sent)
	}
	if _, err := h.Do(ctx, "acme_backup", req); productCode(t, err) != messages.ErrorPluginUnavailable {
		t.Fatalf("a call to a stopped plugin = %v", err)
	}
}

// A refusal retrying cannot change fails the plugin for good; a refusal
// that names a passing condition is retried like a crash.
func TestHandshakeRefusals(t *testing.T) {
	t.Run("final", func(t *testing.T) {
		sp := &fakeSpawner{config: func(int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			c.Init = func(context.Context, *pluginsdk.Plugin) error {
				return pluginsdk.Refuse(extapi.ReasonFailedPrecondition, "ACME_NOT_CONFIGURED", "not configured", nil)
			}
			return c
		}}
		h, _ := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		st := waitStatus(t, h, "acme_backup", inState(StateFailed))
		if st.Code != messages.ErrorPluginHandshakeRefused || st.Params["plugin_code"] != "ACME_NOT_CONFIGURED" {
			t.Fatalf("status = %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
		if n := sp.count(); n != 1 {
			t.Fatalf("a finally refused plugin was started %d times", n)
		}
	})
	t.Run("capability missing", func(t *testing.T) {
		sp := &fakeSpawner{config: func(int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			c.Requires = map[string]extapi.Range{"host.fleet": {Min: 1, Max: 1}}
			return c
		}}
		h, _ := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		st := waitStatus(t, h, "acme_backup", inState(StateFailed))
		if st.Params["plugin_code"] != extapi.ErrorCapabilityMissing {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("passing", func(t *testing.T) {
		sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			if n == 1 {
				c.Init = func(context.Context, *pluginsdk.Plugin) error {
					return pluginsdk.Refuse(extapi.ReasonUnavailable, "ACME_STORE_LOCKED", "locked", nil)
				}
			}
			return c
		}}
		h, log := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		waitStatus(t, h, "acme_backup", inState(StateRunning))
		if got := log.states("acme_backup"); !slices.Contains(got, StateBackoff) || sp.count() != 2 {
			t.Fatalf("states = %v after %d spawns", got, sp.count())
		}
	})
}

// A binary that answers for another build, or picks a capability the
// package was not approved for, never runs.
func TestForeignAnswersFail(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*pluginsdk.Config, *Spec)
		code   string
		field  string
	}{
		"manifest": {func(c *pluginsdk.Config, _ *Spec) {
			c.ManifestSHA256 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		}, messages.ErrorPluginHandshakeMismatch, "manifest_sha256"},
		"version": {func(c *pluginsdk.Config, _ *Spec) { c.Version = "1.0.1" }, messages.ErrorPluginHandshakeMismatch, "version"},
		"capability": {func(c *pluginsdk.Config, s *Spec) {
			c.Uses = map[string]extapi.Range{"table.node_holds": {Min: 1, Max: 1}}
			delete(s.Approved, "table.node_holds")
		}, messages.ErrorPluginCapabilityNotOffered, ""},
	} {
		t.Run(name, func(t *testing.T) {
			spec := testSpec(t, "acme_backup")
			sp := &fakeSpawner{config: func(int) pluginsdk.Config {
				c := sdkConfig("acme_backup")
				s := spec
				tc.mutate(&c, &s)
				return c
			}}
			cfg := sdkConfig("acme_backup")
			tc.mutate(&cfg, &spec)
			h, _ := newTestHost(t, sp, nil, fastTiming())
			_ = h.Start(spec)
			st := waitStatus(t, h, "acme_backup", inState(StateFailed))
			if st.Code != tc.code || (tc.field != "" && st.Params["field"] != tc.field) {
				t.Fatalf("status = %+v", st)
			}
		})
	}
}

// A crashing plugin is restarted after delays that double up to the cap;
// enough crashes in the window put it in crashloop, where retries go on.
func TestCrashBackoffAndCrashloop(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Start = func(context.Context, *pluginsdk.Plugin) error { return errors.New("store corrupt") }
		return c
	}}
	timing := fastTiming()
	timing.CrashloopCount, timing.CrashloopRetry = 5, 300*time.Millisecond
	// Probes are for hangs; keep them out of a test of crashes.
	timing.HealthEvery = time.Minute
	h, log := newTestHost(t, sp, nil, timing)
	_ = h.Start(testSpec(t, "acme_backup"))
	st := waitStatus(t, h, "acme_backup", inState(StateCrashloop))
	if st.Code != messages.ErrorPluginCrashloop || st.Params["crashes"] != 5 || st.Params["retry_s"] != 0 {
		t.Fatalf("status = %+v", st)
	}
	sp.mu.Lock()
	times := append([]time.Time(nil), sp.spawned...)
	sp.mu.Unlock()
	for k, want := range []time.Duration{10, 20, 40, 40} {
		if gap := times[k+1].Sub(times[k]); gap < want*time.Millisecond {
			t.Errorf("restart %d after %v, want at least %v", k+1, gap, want*time.Millisecond)
		}
	}
	if got := log.states("acme_backup"); !slices.Contains(got, StateBackoff) {
		t.Fatalf("states = %v", got)
	}
	n := sp.count()
	deadline := time.Now().Add(5 * time.Second)
	for sp.count() == n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sp.count() == n {
		t.Fatal("a crashlooping plugin was not retried")
	}
	log.mu.Lock()
	all := append([]Status(nil), log.all...)
	log.mu.Unlock()
	for _, s := range all {
		if s.State == StateBackoff && s.Code != messages.ErrorPluginCrashed {
			t.Fatalf("backoff names %s, want the crash", s.Code)
		}
	}
}

// A plugin that stops answering health is killed after the misses and
// restarted; a degraded answer shows its code until it recovers.
func TestHealth(t *testing.T) {
	t.Run("hung", func(t *testing.T) {
		sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			if n == 1 {
				c.Health = func(ctx context.Context) extapi.Health { <-ctx.Done(); return extapi.Health{Status: extapi.HealthOK} }
			}
			return c
		}}
		h, log := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.Starts == 2 })
		if sent := sp.proc(1).sent(); !slices.Contains(sent, os.Signal(syscall.SIGKILL)) {
			t.Fatalf("the hung plugin got %v", sent)
		}
		found := false
		log.mu.Lock()
		defer log.mu.Unlock()
		for _, s := range log.all {
			found = found || (s.State == StateBackoff && s.Code == messages.ErrorPluginUnhealthy && s.Params["misses"] == 3)
		}
		if !found {
			t.Fatal("no backoff named PLUGIN_UNHEALTHY with 3 misses")
		}
	})
	t.Run("degraded", func(t *testing.T) {
		var degraded atomic.Bool
		degraded.Store(true)
		sp := &fakeSpawner{config: func(int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			c.Health = func(context.Context) extapi.Health {
				if degraded.Load() {
					return extapi.Health{Status: extapi.HealthDegraded, Code: "ACME_STORE_LOCKED", Params: extapi.Params{"retries": 2}}
				}
				return extapi.Health{Status: extapi.HealthOK}
			}
			return c
		}}
		h, _ := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		st := waitStatus(t, h, "acme_backup", inState(StateDegraded))
		if st.Code != "ACME_STORE_LOCKED" || !st.State.Up() {
			t.Fatalf("status = %+v", st)
		}
		degraded.Store(false)
		waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.Code == "" })
	})
}

// A plugin that ignores shutdown gets SIGTERM, then SIGKILL.
func TestStopEscalates(t *testing.T) {
	sp := &fakeSpawner{linger: true, ignoreTerm: true, config: func(int) pluginsdk.Config { return sdkConfig("acme_backup") }}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	start := time.Now()
	if err := h.Stop(context.Background(), "acme_backup"); err != nil {
		t.Fatal(err)
	}
	if sent := sp.proc(1).sent(); !slices.Equal(sent, []os.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("signals = %v", sent)
	}
	if took := time.Since(start); took < 400*time.Millisecond {
		t.Fatalf("stop escalated after %v, before the graces", took)
	}
}

// An on-demand plugin starts on its first call and stops when idle; it
// may not push state.
func TestOnDemand(t *testing.T) {
	state := &fakeState{}
	pushErr := make(chan error, 1)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_driver")
		c.Start = func(ctx context.Context, p *pluginsdk.Plugin) error {
			_, err := p.PushState(ctx, extapi.KindBanners, banners("x"))
			pushErr <- err
			return nil
		}
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
		return c
	}}
	timing := fastTiming()
	timing.IdleStop = 200 * time.Millisecond
	h, log := newTestHost(t, sp, state, timing)
	spec := testSpec(t, "acme_driver")
	spec.Lifecycle = extapi.LifecycleOnDemand
	_ = h.Start(spec)
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	if sp.count() != 0 {
		t.Fatal("an on-demand plugin started before it was called")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequest(http.MethodPost, "/gwx/v1/svc/provider.list", nil)
	resp, err := h.Do(ctx, "acme_driver", req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" || sp.count() != 1 {
		t.Fatalf("body %q after %d spawns", body, sp.count())
	}
	var e *pluginsdk.Error
	if err := <-pushErr; !errors.As(err, &e) || e.Wire.Code != messages.ErrorPluginStateOnDemand || e.Status != http.StatusForbidden {
		t.Fatalf("on-demand push = %v", err)
	}
	if len(state.recorded()) != 0 {
		t.Fatal("an on-demand push reached the state host")
	}
	waitStatus(t, h, "acme_driver", func(s Status) bool { return s.State == StateIdle && s.Starts == 1 })
	if got := log.states("acme_driver"); !slices.Contains(got, StateRunning) {
		t.Fatalf("states = %v", got)
	}
	if sent := sp.proc(1).sent(); len(sent) != 0 {
		t.Fatalf("an idle plugin that obeyed shutdown was signalled: %v", sent)
	}
}

// Calls beyond the plugin's concurrency are refused at once, while its
// health probes still get through.
func TestBusyPluginRefusesWithoutBlocking(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			entered <- struct{}{}
			<-release
		})
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	spec := testSpec(t, "acme_backup")
	spec.MaxConcurrency = 1
	_ = h.Start(spec)
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		req, _ := http.NewRequest(http.MethodGet, "/r/slow", nil)
		if resp, err := h.Do(ctx, "acme_backup", req); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	req, _ := http.NewRequest(http.MethodGet, "/r/other", nil)
	if _, err := h.Do(ctx, "acme_backup", req); productCode(t, err) != messages.ErrorPluginBusy {
		t.Fatalf("second call = %v", err)
	}
	time.Sleep(100 * time.Millisecond) // several health periods
	if st, _ := h.Status("acme_backup"); st.State != StateRunning || st.Starts != 1 {
		t.Fatalf("a busy plugin was treated as unhealthy: %+v", st)
	}
	close(release)
}

// Spawn failures and the trampoline's own exit codes become the status
// the console shows.
func TestStartFailures(t *testing.T) {
	t.Run("spawn error", func(t *testing.T) {
		sp := &fakeSpawner{spawnErr: func(int) error { return errors.New("fork: resource temporarily unavailable") },
			config: func(int) pluginsdk.Config { return sdkConfig("acme_backup") }}
		h, _ := newTestHost(t, sp, nil, fastTiming())
		_ = h.Start(testSpec(t, "acme_backup"))
		st := waitStatus(t, h, "acme_backup", inState(StateBackoff))
		if st.Code != messages.ErrorPluginStartFailed {
			t.Fatalf("status = %+v", st)
		}
	})
	t.Run("isolation required", func(t *testing.T) {
		old := landlockABI
		landlockABI = func() int { return 0 }
		defer func() { landlockABI = old }()
		sp := &fakeSpawner{config: func(int) pluginsdk.Config { return sdkConfig("acme_backup") }}
		h, _ := newTestHost(t, sp, nil, fastTiming())
		spec := testSpec(t, "acme_backup")
		spec.RequireLandlock = true
		_ = h.Start(spec)
		st := waitStatus(t, h, "acme_backup", inState(StateFailed))
		if st.Code != messages.ErrorPluginIsolationUnavailable || st.Isolation != "none" || sp.count() != 0 {
			t.Fatalf("status = %+v after %d spawns", st, sp.count())
		}
	})
	if messages.ErrorPluginIsolationUnavailable != pluginexec.IsolationCode {
		t.Fatal("the trampoline and the console name the isolation refusal differently")
	}
}

func TestExitOutcomes(t *testing.T) {
	for _, tc := range []struct {
		exit   ExitStatus
		before bool
		kind   outcomeKind
		code   string
	}{
		{ExitStatus{Code: pluginexec.ExitIsolationUnavailable}, true, outcomeFailed, messages.ErrorPluginIsolationUnavailable},
		{ExitStatus{Code: pluginexec.ExitUsage}, true, outcomeFailed, messages.ErrorPluginStartFailed},
		{ExitStatus{Code: pluginexec.ExitExecFailed}, true, outcomeCrash, messages.ErrorPluginStartFailed},
		{ExitStatus{Code: pluginexec.ExitIsolationUnavailable}, false, outcomeCrash, messages.ErrorPluginCrashed},
		{ExitStatus{Code: -1, Signal: "killed"}, true, outcomeCrash, messages.ErrorPluginCrashed},
	} {
		o := exitOutcome("acme", tc.exit, tc.before)
		if o.kind != tc.kind || o.code != tc.code {
			t.Errorf("%+v before=%v: %+v", tc.exit, tc.before, o)
		}
	}
	tm := Timing{}.withDefaults()
	var got []time.Duration
	for n := 1; n <= 11; n++ {
		got = append(got, tm.backoff(n))
	}
	if got[0] != time.Second || got[1] != 2*time.Second || got[8] != 256*time.Second || got[9] != 5*time.Minute || got[10] != 5*time.Minute {
		t.Fatalf("backoff = %v", got)
	}
}

func TestSpecValidation(t *testing.T) {
	h, _ := newTestHost(t, &fakeSpawner{config: func(int) pluginsdk.Config { return sdkConfig("x") }}, nil, fastTiming())
	for name, mutate := range map[string]func(*Spec){
		"id":        func(s *Spec) { s.ID = "Acme" },
		"grant":     func(s *Spec) { s.Grant.Version = "2.0.0" },
		"digest":    func(s *Spec) { s.ManifestSHA256 = "x" },
		"relative":  func(s *Spec) { s.Exe = "bin/acme" },
		"lifecycle": func(s *Spec) { s.Lifecycle = "sometimes" },
		"limits":    func(s *Spec) { s.MaxConcurrency = 5000 },
	} {
		s := testSpec(t, "acme_backup")
		mutate(&s)
		if err := h.Start(s); err == nil {
			t.Errorf("%s: started %+v", name, s)
		}
	}
	if _, err := New(Config{}); err == nil {
		t.Error("a host without Core was built")
	}
	if err := h.Stop(context.Background(), "nobody"); !errors.Is(err, ErrUnknownPlugin) {
		t.Errorf("stop of an unknown plugin = %v", err)
	}
}
