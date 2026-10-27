//go:build cloudgpu

package l4

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Offline: go test -tags=cloudgpu -run 'TestJudgeProviderInstallCheck|TestWaitQueuedDialNodeOnline' ./test/l4/

func TestJudgeProviderInstallCheck(t *testing.T) {
	const queued = "queued for reconnection: read server_dial challenge: read tcp 100.96.0.20:41800->213.173.108.41:22256: i/o timeout"
	cases := []struct {
		name       string
		want       string
		check      provision.CheckResult
		accepted   bool
		queuedDial bool
	}{
		{"server_dial ok", "server_dial", provision.CheckResult{Name: "server_dial", Status: "ok", Detail: "connected to 1.2.3.4:5"}, true, false},
		{"server_dial queued for reconnection", "server_dial", provision.CheckResult{Name: "server_dial", Status: "warn", Detail: queued}, true, true},
		{"server_dial warn for another reason", "server_dial", provision.CheckResult{Name: "server_dial", Status: "warn", Detail: "server_dial address is empty"}, false, false},
		{"server_dial not configured", "server_dial", provision.CheckResult{Name: "server_dial", Status: "warn", Detail: "server_dial is not configured"}, false, false},
		{"server_dial error even with the queued text", "server_dial", provision.CheckResult{Name: "server_dial", Status: "error", Detail: queued}, false, false},
		{"queued text on the wrong check", "agent_dial", provision.CheckResult{Name: "agent_dial", Status: "warn", Detail: queued}, false, false},
		{"queued text under another name", "server_dial", provision.CheckResult{Name: "agent_dial", Status: "warn", Detail: queued}, false, false},
		{"agent_dial ok", "agent_dial", provision.CheckResult{Name: "agent_dial", Status: "ok"}, true, false},
		{"agent_dial waiting for first session", "agent_dial", provision.CheckResult{Name: "agent_dial", Status: "warn", Detail: "agent started; waiting for its first session"}, true, false},
		{"agent_dial other warn", "agent_dial", provision.CheckResult{Name: "agent_dial", Status: "warn", Detail: "no session"}, false, false},
		{"precheck warn", "precheck", provision.CheckResult{Name: "precheck", Status: "warn", Detail: "Docker missing"}, true, false},
		{"precheck error", "precheck", provision.CheckResult{Name: "precheck", Status: "error"}, false, false},
		{"install agent warn", "install agent", provision.CheckResult{Name: "install agent", Status: "warn"}, false, false},
		{"wrong check at the position", "install agent", provision.CheckResult{Name: "precheck", Status: "ok"}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := judgeProviderInstallCheck(tc.want, tc.check)
			if got.Accepted != tc.accepted || got.QueuedDial != tc.queuedDial {
				t.Fatalf("judgeProviderInstallCheck(%q, %+v) = accepted %v queued %v, want accepted %v queued %v", tc.want, tc.check, got.Accepted, got.QueuedDial, tc.accepted, tc.queuedDial)
			}
		})
	}
}

func TestWaitQueuedDialNodeOnline(t *testing.T) {
	const queued = "queued for reconnection: read server_dial challenge: i/o timeout"
	t.Run("comes online after some redials", func(t *testing.T) {
		polls := 0
		state := func() (string, error) {
			polls++
			if polls < 3 {
				return store.NodeStateOffline, nil
			}
			return store.NodeStateOnline, nil
		}
		probe := func() string { t.Fatal("probe must not run when the node comes online"); return "" }
		if err := waitQueuedDialNodeOnline(context.Background(), time.Second, time.Millisecond, state, probe, queued); err != nil {
			t.Fatal(err)
		}
		if polls != 3 {
			t.Fatalf("polls = %d, want 3", polls)
		}
	})
	t.Run("never online reports the dial error, state and probe", func(t *testing.T) {
		state := func() (string, error) { return store.NodeStateOffline, nil }
		probe := func() string { return "dial 1.2.3.4:5: i/o timeout" }
		err := waitQueuedDialNodeOnline(context.Background(), 30*time.Millisecond, time.Millisecond, state, probe, queued)
		if err == nil {
			t.Fatal("a node that never comes online passed")
		}
		for _, want := range []string{queued, `node state "offline"`, "dial 1.2.3.4:5: i/o timeout", "within 30ms"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q lacks %q", err, want)
			}
		}
	})
	t.Run("unreadable node state is reported", func(t *testing.T) {
		state := func() (string, error) { return "", errors.New("record not found") }
		err := waitQueuedDialNodeOnline(context.Background(), 20*time.Millisecond, time.Millisecond, state, func() string { return "p" }, queued)
		if err == nil || !strings.Contains(err.Error(), "record not found") {
			t.Fatalf("err = %v, want the state read error", err)
		}
	})
	t.Run("canceled context stops the wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		state := func() (string, error) { return store.NodeStateOffline, nil }
		err := waitQueuedDialNodeOnline(ctx, time.Minute, time.Millisecond, state, func() string { return "p" }, queued)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), queued) {
			t.Fatalf("err = %v, want context.Canceled carrying the install dial error", err)
		}
	})
}
