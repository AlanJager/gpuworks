package pluginhost

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

func productParams(t *testing.T, err error) (string, map[string]any) {
	t.Helper()
	var pe *messages.ProductError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want a product error", err)
	}
	return pe.Code(), pe.Params()
}

func callPlugin(ctx context.Context, h *Host, id, path string, class Class) (*http.Response, error) {
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	return h.Do(ctx, id, req, class)
}

func okHandler(id string) func(int) pluginsdk.Config {
	return func(int) pluginsdk.Config {
		c := sdkConfig(id)
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
		return c
	}
}

func onDemandSpec(t *testing.T, id string) Spec {
	spec := testSpec(t, id)
	spec.Lifecycle = extapi.LifecycleOnDemand
	return spec
}

// A hundred calls at once to an idle on-demand plugin start it once, and
// every one of them is served.
func TestConcurrentCallsToAnIdlePluginStartItOnce(t *testing.T) {
	sp := &fakeSpawner{config: okHandler("acme_driver")}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	spec := onDemandSpec(t, "acme_driver")
	spec.MaxConcurrency = 200 // members may hold 150
	_ = h.Start(spec)
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Go(func() {
			resp, err := callPlugin(ctx, h, "acme_driver", "/r/x", Class{})
			if err != nil {
				errs <- err
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if string(body) != "ok" {
				errs <- errors.New("body " + string(body))
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("call: %v", err)
	}
	if n := sp.count(); n != 1 {
		t.Fatalf("%d spawns, want 1", n)
	}
}

// Calls that keep arriving around the moment an on-demand plugin goes
// idle are all served: each either gets the running plugin or waits for
// the next start, and none is refused.
func TestCallsRacingTheIdleStopAreServed(t *testing.T) {
	sp := &fakeSpawner{config: okHandler("acme_driver")}
	timing := fastTiming()
	timing.IdleStop = 20 * time.Millisecond
	h, _ := newTestHost(t, sp, nil, timing)
	_ = h.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for n := range 40 {
		resp, err := callPlugin(ctx, h, "acme_driver", "/r/x", Class{})
		if err != nil {
			t.Fatalf("call %d: %v", n, err)
		}
		resp.Body.Close()
		time.Sleep(time.Duration(15+n%3*5) * time.Millisecond)
	}
	if sp.count() < 2 {
		t.Fatalf("%d spawns: the plugin never went idle between calls", sp.count())
	}
}

// A cold start that outlasts the call's deadline is a timeout that says
// the plugin was still starting; the start goes on and the next call is
// served.
func TestAColdStartPastTheDeadlineTimesOut(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := okHandler("acme_driver")(0)
		c.Init = func(context.Context, *pluginsdk.Plugin) error { time.Sleep(300 * time.Millisecond); return nil }
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	_ = h.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := callPlugin(ctx, h, "acme_driver", "/r/x", Class{})
	code, params := productParams(t, err)
	if code != messages.ErrorPluginTimeout || params["state"] != "starting" || params["timeout_s"] != 1 || params["id"] != "acme_driver" {
		t.Fatalf("cold start past the deadline = %s %v", code, params)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the timeout does not wrap the deadline: %v", err)
	}
	waitStatus(t, h, "acme_driver", inState(StateRunning))
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	resp, err := callPlugin(ctx2, h, "acme_driver", "/r/x", Class{})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sp.count() != 1 {
		t.Fatalf("%d spawns, want 1", sp.count())
	}
}

// A running plugin that does not answer within the deadline is a timeout
// that says it was running.
func TestARunningPluginPastTheDeadlineTimesOut(t *testing.T) {
	hang := make(chan struct{})
	defer close(hang)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Handler = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-hang })
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_, err := callPlugin(ctx, h, "acme_backup", "/r/slow", Class{})
	if code, params := productParams(t, err); code != messages.ErrorPluginTimeout || params["state"] != "running" || params["timeout_s"] != 2 {
		t.Fatalf("slow call = %s %v", code, params)
	}
}

// A plugin that dies with the request in hand fails the call as possibly
// applied; a connection that was gone before the request was written
// refuses it as unavailable, safe to retry.
func TestCrashBeforeAndAfterTheRequestWasSent(t *testing.T) {
	entered := make(chan struct{}, 1)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Handler = http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-r.Context().Done()
		})
		return c
	}}
	timing := fastTiming()
	timing.HealthEvery = time.Hour // the broken connection below must not be probed away
	h, _ := newTestHost(t, sp, nil, timing)
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		<-entered
		_ = sp.proc(1).Signal(syscall.SIGKILL)
	}()
	_, err := callPlugin(ctx, h, "acme_backup", "/r/apply", Class{})
	if code, params := productParams(t, err); code != messages.ErrorPluginRouteFailed || params["id"] != "acme_backup" {
		t.Fatalf("crash after the request was sent = %s %v", code, params)
	}

	waitStatus(t, h, "acme_backup", func(s Status) bool { return s.State == StateRunning && s.Starts == 2 })
	conn := h.get("acme_backup").currentRun().conn
	_ = sp.proc(2).serve.Close()
	for deadline := time.Now().Add(5 * time.Second); conn.cc.CanTakeNewRequest(); {
		if time.Now().After(deadline) {
			t.Fatal("the closed connection still takes requests")
		}
		time.Sleep(time.Millisecond)
	}
	_, err = callPlugin(ctx, h, "acme_backup", "/r/apply", Class{})
	if code, params := productParams(t, err); code != messages.ErrorPluginUnavailable || params["plugin"] != "acme_backup" || params["state"] != "running" {
		t.Fatalf("a call on a connection gone before the request = %s %v", code, params)
	}
	select {
	case <-entered:
		t.Fatal("the refused call reached the plugin")
	default:
	}
}

// Open streams do not take the slots of short calls, and member calls
// leave the admin reserve free.
func TestSlotPoolsAndTheAdminReserve(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 16)
	sp := &fakeSpawner{config: func(int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/r/fast" {
				_, _ = io.WriteString(w, "ok")
				return
			}
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			entered <- struct{}{}
			<-release
		})
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	spec := testSpec(t, "acme_backup")
	spec.MaxConcurrency = 4 // a reserve of 1: members may hold 3
	_ = h.Start(spec)
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var open []*http.Response
	defer func() {
		close(release)
		for _, r := range open {
			r.Body.Close()
		}
	}()
	hold := func(class Class) {
		t.Helper()
		resp, err := callPlugin(ctx, h, "acme_backup", "/r/hold", class)
		if err != nil {
			t.Fatalf("hold %+v: %v", class, err)
		}
		<-entered
		open = append(open, resp)
	}
	// Every stream slot is taken, by members up to their share and an
	// administrator for the rest.
	for range 3 {
		hold(Class{Stream: true})
	}
	if _, err := callPlugin(ctx, h, "acme_backup", "/r/hold", Class{Stream: true}); err == nil {
		t.Fatal("a member stream took the admin reserve")
	} else if code, params := productParams(t, err); code != messages.ErrorPluginBusy || params["limit"] != 3 {
		t.Fatalf("member stream over its share = %s %v", code, params)
	}
	hold(Class{Stream: true, Admin: true})
	if _, err := callPlugin(ctx, h, "acme_backup", "/r/hold", Class{Stream: true, Admin: true}); err == nil {
		t.Fatal("a fifth stream was let in")
	} else if code, params := productParams(t, err); code != messages.ErrorPluginBusy || params["limit"] != 4 {
		t.Fatalf("admin stream over the pool = %s %v", code, params)
	}
	// Short calls still get through.
	resp, err := callPlugin(ctx, h, "acme_backup", "/r/fast", Class{})
	if err != nil {
		t.Fatalf("a short call behind four open streams: %v", err)
	}
	resp.Body.Close()
	// Members fill their share of the short-call pool; an administrator
	// still gets the reserved slot.
	for range 3 {
		hold(Class{})
	}
	if _, err := callPlugin(ctx, h, "acme_backup", "/r/fast", Class{}); err == nil {
		t.Fatal("a member call took the admin reserve")
	}
	resp, err = callPlugin(ctx, h, "acme_backup", "/r/fast", Class{Admin: true})
	if err != nil {
		t.Fatalf("an admin call with members holding their share: %v", err)
	}
	resp.Body.Close()
}

func TestAdminReserve(t *testing.T) {
	for n, want := range map[int]int{1: 0, 2: 1, 4: 1, 7: 1, 8: 2, 100: 25} {
		if got := AdminReserve(n); got != want {
			t.Errorf("AdminReserve(%d) = %d, want %d", n, got, want)
		}
	}
}

// A call to a crashlooping on-demand plugin is refused at once, without
// waiting out the slow retry.
func TestACrashloopingOnDemandPluginIsRefusedAtOnce(t *testing.T) {
	sp := &fakeSpawner{config: okHandler("acme_driver"), spawnErr: func(int) error { return errors.New("exec format error") }}
	timing := fastTiming()
	timing.CrashloopCount, timing.CrashloopRetry = 1, time.Hour
	h, _ := newTestHost(t, sp, nil, timing)
	_ = h.Start(onDemandSpec(t, "acme_driver"))
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := callPlugin(ctx, h, "acme_driver", "/r/x", Class{})
	if code, params := productParams(t, err); code != messages.ErrorPluginUnavailable || params["state"] != "crashloop" {
		t.Fatalf("call to a crashlooping plugin = %s %v", code, params)
	}
	start := time.Now()
	_, err = callPlugin(ctx, h, "acme_driver", "/r/x", Class{})
	if code, _ := productParams(t, err); code != messages.ErrorPluginUnavailable || time.Since(start) > time.Second {
		t.Fatalf("second call = %s after %s", code, time.Since(start))
	}
}
