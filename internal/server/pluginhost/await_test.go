package pluginhost

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// A plugin that comes up and stays up passes the gate once hold has
// passed, not before.
func TestAwaitHealthyPassesAfterTheHold(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config { return sdkConfig("acme_backup") }}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	if err := h.Start(testSpec(t, "acme_backup")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := h.AwaitHealthy(context.Background(), "acme_backup", 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took < 300*time.Millisecond {
		t.Fatalf("the gate passed after %v, before its hold", took)
	}
	if st, _ := h.Status("acme_backup"); st.State != StateRunning || st.Starts != 1 {
		t.Fatalf("status after the gate = %+v", st)
	}
	if err := h.AwaitHealthy(context.Background(), "acme_other", time.Millisecond); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("gate of an unsupervised plugin = %v", err)
	}
}

// A plugin that crashes fails the gate at once, long before the hold,
// with the crash's code; one refused at its handshake with that code.
func TestAwaitHealthyFailsFastOnACrash(t *testing.T) {
	for name, c := range map[string]struct {
		config func(n int) pluginsdk.Config
		code   string
	}{
		"crash after start": {func(int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			c.Start = func(context.Context, *pluginsdk.Plugin) error { return errors.New("store corrupt") }
			return c
		}, messages.ErrorPluginCrashed},
		"exit in the handshake": {func(int) pluginsdk.Config {
			c := sdkConfig("acme_backup")
			c.Init = func(context.Context, *pluginsdk.Plugin) error { return errors.New("cannot open its store") }
			return c
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			sp := &fakeSpawner{config: c.config}
			timing := fastTiming()
			timing.HealthEvery = time.Minute
			h, _ := newTestHost(t, sp, nil, timing)
			_ = h.Start(testSpec(t, "acme_backup"))
			start := time.Now()
			err := h.AwaitHealthy(context.Background(), "acme_backup", time.Minute)
			var ue *UnhealthyError
			if !errors.As(err, &ue) || ue.Timeout || ue.Status.State.Up() {
				t.Fatalf("gate = %v", err)
			}
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("the gate took %v to see the crash", took)
			}
			if c.code != "" && ue.Code() != c.code {
				t.Fatalf("code = %s, want %s", ue.Code(), c.code)
			}
			if ue.Code() == "" {
				t.Fatal("the failure names no code")
			}
		})
	}
}

// A plugin still starting when the context ends fails the gate as a
// timeout; a stopped one fails it as unavailable.
func TestAwaitHealthyTimesOutAndSeesAStop(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		if n == 1 {
			c.Init = func(ctx context.Context, _ *pluginsdk.Plugin) error {
				select {
				case <-block:
				case <-ctx.Done():
				}
				return nil
			}
		}
		return c
	}}
	timing := fastTiming()
	timing.InitTimeout = time.Minute
	h, _ := newTestHost(t, sp, nil, timing)
	_ = h.Start(testSpec(t, "acme_backup"))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var ue *UnhealthyError
	if err := h.AwaitHealthy(ctx, "acme_backup", time.Minute); !errors.As(err, &ue) || !ue.Timeout || ue.Code() != messages.ErrorPluginTimeout {
		t.Fatalf("gate of a plugin still starting = %v", err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- h.AwaitHealthy(context.Background(), "acme_backup", time.Minute) }()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer stopCancel()
	if err := h.Stop(stopCtx, "acme_backup"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stopped:
		if !errors.As(err, &ue) || ue.Timeout || ue.Code() != messages.ErrorPluginUnavailable {
			t.Fatalf("gate of a stopped plugin = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the gate did not see the stop")
	}
}

// An on-demand plugin is started by the gate, and passes once it came up,
// even though it goes idle again during the hold.
func TestAwaitHealthyStartsAnOnDemandPlugin(t *testing.T) {
	sp := &fakeSpawner{config: func(int) pluginsdk.Config { return sdkConfig("acme_driver") }}
	timing := fastTiming()
	timing.IdleStop = 50 * time.Millisecond
	h, _ := newTestHost(t, sp, nil, timing)
	spec := testSpec(t, "acme_driver")
	spec.Lifecycle = extapi.LifecycleOnDemand
	_ = h.Start(spec)
	waitStatus(t, h, "acme_driver", inState(StateIdle))
	if err := h.AwaitHealthy(context.Background(), "acme_driver", 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if sp.count() != 1 {
		t.Fatalf("%d processes started, want the one the gate asked for", sp.count())
	}
}
