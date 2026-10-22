// Package expose_test drives the public entry point against the real server
// assembly: the server records the address it bound, the connector service
// (expose.RunService) forwards to it, and the connector is a real process that
// does not depend on the server staying up.
package expose_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	exposelib "gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestConnectorHostHelperProcess is this binary's connector-host entry point.
// The real assembly spawns `gpuworks expose connector-host`, which a test
// binary cannot run, so the harness points the supervisor at this instead —
// the code under it is the same HostMain production runs.
func TestConnectorHostHelperProcess(t *testing.T) {
	if os.Getenv(exposelib.HostSpecEnv) == "" {
		t.Skip("entry point for the harness connector host")
	}
	os.Exit(exposelib.HostMain(context.Background(), os.Stdin))
}

// The connector behaves like a quick tunnel: it reports its pid, prints the two
// lines a real one prints, and stays alive until it is stopped.
const fakeCloudflared = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
echo "$$" > "$FAKE_CF_PIDFILE"
echo 'INF |  Your quick Tunnel has been created! Visit it at:  |'
echo 'INF |  https://assembly-check.trycloudflare.com  |'
echo 'INF Registered tunnel connection connIndex=0 event=0 ip=198.41.192.1 location=lax99 protocol=quic'
while true; do sleep 1; done
`

// healthyChecker stands in for the public and local probes: readiness here is
// about the assembly wiring, and the probe's own behaviour has its own tests.
type healthyChecker struct{}

func (healthyChecker) CheckPublic(context.Context, string) error { return nil }
func (healthyChecker) CheckLocal(context.Context, string) error  { return nil }

func TestExposeAssemblyLifecycle(t *testing.T) {
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "cloudflared"), []byte(fakeCloudflared), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(binDir, "connector.pid")
	t.Setenv("FAKE_CF_PIDFILE", pidFile)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The two seams a harness needs: its own helper entry point, and fakes for
	// the process host's dependencies.
	restoreDeps, restoreHelper := exposelib.DepsFactory, exposelib.SelfHelper
	exposelib.SelfHelper = func() exposelib.HelperCommand {
		return exposelib.HelperCommand{Path: os.Args[0], Args: []string{"-test.run=TestConnectorHostHelperProcess"}}
	}
	exposelib.DepsFactory = func() exposelib.Deps {
		deps := exposelib.NewDeps()
		deps.Helper = exposelib.SelfHelper()
		deps.Prober = healthyChecker{}
		return deps
	}
	t.Cleanup(func() {
		exposelib.DepsFactory = restoreDeps
		exposelib.SelfHelper = restoreHelper
	})

	env := testenv.New(t)
	if !strings.HasPrefix(env.URL(), "http://127.0.0.1:") {
		t.Fatalf("harness URL = %q", env.URL())
	}
	store := exposelib.NewStore(env.DataDir())

	// The server's whole part: record the address it really bound.
	waitFor(t, 30*time.Second, "the server to record its address", func() bool {
		_, err := os.Stat(store.ServerOriginPath())
		return err == nil
	})
	origin, how, err := exposelib.ResolveOrigin(store, "")
	if err != nil {
		t.Fatalf("ResolveOrigin: %v", err)
	}
	if origin != env.URL() || how != "recorded by the server" {
		t.Fatalf("origin = %q (%s), want the harness listener %q recorded by the server", origin, how, env.URL())
	}

	// The CLI's part: commit the request and start the connector service.
	if err := store.WriteConfig(&exposelib.Config{
		Revision: "rev-1", Provider: exposelib.ProviderQuick, Desired: exposelib.DesiredEnabled, Origin: origin,
	}); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- exposelib.RunService(context.Background(), store, nil) }()
	t.Cleanup(func() {
		// Whatever the outcome, nothing may keep serving after the test.
		_ = store.WriteConfig(&exposelib.Config{Revision: "cleanup", Provider: exposelib.ProviderQuick, Desired: exposelib.DesiredDisabled})
		select {
		case <-serviceDone:
		case <-time.After(60 * time.Second):
			t.Error("the connector service did not exit")
		}
	})

	status := waitForState(t, store, "rev-1", exposelib.StateReady, 60*time.Second)
	if status.URL != "https://assembly-check.trycloudflare.com" {
		t.Fatalf("status URL = %q, want what the connector printed", status.URL)
	}
	if status.LocalOrigin != env.URL() {
		t.Fatalf("local origin = %q, want the address the server recorded %q", status.LocalOrigin, env.URL())
	}
	connectorPID := readPID(t, pidFile)
	if !processAlive(connectorPID) {
		t.Fatalf("the connector (pid %d) should be running", connectorPID)
	}
	if !store.SupervisorHeld() {
		t.Error("the connector service should hold the hosting lock")
	}

	// Stopping the server is not the connector's business: same connector, same
	// address, still hosted.
	env.StopServer(t)
	time.Sleep(500 * time.Millisecond)
	if !processAlive(connectorPID) || readPID(t, pidFile) != connectorPID {
		t.Fatal("stopping the server took the connector with it")
	}
	if st, _ := store.ReadStatus(); st == nil || st.URL != status.URL || !store.SupervisorHeld() {
		t.Fatalf("status after the server stopped = %+v, want the same address, still hosted", st)
	}

	// down: a new revision asking for disabled stops the connector, and the
	// service exits because nothing is asked for any more.
	if err := store.WriteConfig(&exposelib.Config{
		Revision: "rev-2", Provider: exposelib.ProviderQuick, Desired: exposelib.DesiredDisabled,
	}); err != nil {
		t.Fatalf("WriteConfig(disabled): %v", err)
	}
	stopped := waitForState(t, store, "rev-2", exposelib.StateDisabled, 60*time.Second)
	if stopped.URL != "" {
		t.Errorf("a disabled entry point must not carry an address: %q", stopped.URL)
	}
	waitFor(t, 30*time.Second, "the connector to exit", func() bool { return !processAlive(connectorPID) })
	select {
	case err := <-serviceDone:
		serviceDone <- err // for the cleanup
		if err != nil {
			t.Fatalf("RunService: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the connector service did not exit once the entry point was disabled")
	}
	if store.SupervisorHeld() {
		t.Error("the hosting lock outlived the connector service")
	}
}

func waitForState(t *testing.T, store *exposelib.Store, revision, state string, timeout time.Duration) *exposelib.Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *exposelib.Status
	for time.Now().Before(deadline) {
		st, _ := store.ReadStatus()
		last = st
		if st != nil && st.Revision == revision && st.State == state {
			return st
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("state %q of revision %q was not reached; last status: %+v", state, revision, last)
	return nil
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the connector never reported a pid in %s", path)
	return 0
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
