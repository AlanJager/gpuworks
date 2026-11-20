//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/restorehold"
)

const controllerStartLog = "starting controller on local database leader"

type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncLog) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncLog) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func recoverySnapshot(a *App) (bool, DisasterRecovery) {
	a.disasterMu.Lock()
	defer a.disasterMu.Unlock()
	if a.disaster == nil {
		return false, DisasterRecovery{}
	}
	return true, *a.disaster
}

func requireNoControllerStart(t *testing.T, logs *syncLog, wait time.Duration) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if n := strings.Count(logs.String(), controllerStartLog); n != 0 {
			t.Fatalf("controller started while recovery was closed:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func waitControllerStart(t *testing.T, logs *syncLog) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(logs.String(), controllerStartLog) >= 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("controller did not start:\n%s", logs.String())
}

func stopController(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller supervisor did not stop")
	}
}

// TestRestoredAppKeepsSchedulingClosedUntilTheOldMemberIsIsolated drives New
// and runController, the startup path a restored process actually uses.
func TestRestoredAppKeepsSchedulingClosedUntilTheOldMemberIsIsolated(t *testing.T) {
	dir := t.TempDir()
	if err := restorehold.MarkSchedulingIsolationRequired(dir); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Addr: "127.0.0.1:0", DataDir: dir, DBPath: filepath.Join(dir, "gpuworks.db"), AdminUser: "admin", AdminPassword: "gpuworks-test-password-2026"}
	logs := &syncLog{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	a, err := New(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	present, state := recoverySnapshot(a)
	if !present || state.Isolated || state.Scheduling {
		t.Fatalf("restored app did not start closed: present=%v state=%+v", present, state)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.runController(ctx)
	}()
	// The shipped supervisor observes immediately and then once a second.
	// A cluster-id change that opened scheduling would log on the next tick.
	requireNoControllerStart(t, logs, 2500*time.Millisecond)
	a.NoteRestoredClusterID("replacement-cluster")
	requireNoControllerStart(t, logs, 2500*time.Millisecond)
	present, state = recoverySnapshot(a)
	if !present || state.Isolated || state.Scheduling {
		stopController(t, cancel, done)
		t.Fatalf("cluster id opened scheduling: present=%v state=%+v", present, state)
	}
	if err := a.ConfirmRestoredMemberIsolated(); err != nil {
		stopController(t, cancel, done)
		t.Fatal(err)
	}
	if required, err := restorehold.SchedulingIsolationRequired(dir); err != nil || required {
		stopController(t, cancel, done)
		t.Fatalf("isolation hold still on disk: required=%v err=%v", required, err)
	}
	waitControllerStart(t, logs)
	stopController(t, cancel, done)
	if n := strings.Count(logs.String(), controllerStartLog); n != 1 {
		t.Fatalf("controller starts=%d\n%s", n, logs.String())
	}
	present, state = recoverySnapshot(a)
	if !present || !state.Isolated || !state.Scheduling {
		t.Fatalf("confirmed isolation did not resume scheduling: present=%v state=%+v", present, state)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	ordinaryLogs := &syncLog{}
	ordinary, err := New(cfg, slog.New(slog.NewTextHandler(ordinaryLogs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ordinary.Close() })
	if present, state := recoverySnapshot(ordinary); present {
		t.Fatalf("next start stayed in recovery: %+v", state)
	}
	ordinaryCtx, ordinaryCancel := context.WithCancel(t.Context())
	ordinaryDone := make(chan struct{})
	go func() {
		defer close(ordinaryDone)
		ordinary.runController(ordinaryCtx)
	}()
	waitControllerStart(t, ordinaryLogs)
	stopController(t, ordinaryCancel, ordinaryDone)
	if n := strings.Count(ordinaryLogs.String(), controllerStartLog); n != 1 {
		t.Fatalf("ordinary leadership starts=%d\n%s", n, ordinaryLogs.String())
	}
}
