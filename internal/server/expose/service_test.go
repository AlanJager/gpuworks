package expose

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The connector service runs apart from the server: it must ride out a server
// that is down, and it must go away by itself once nothing is asked for.

func (f *fakeChecker) setLocalErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.localErr = err
}

// TestSupervisorKeepsTheConnectorWhileTheServerIsDown: the server restarts on
// its own schedule. A connector that started while the server was down, or
// that sees the server go away, keeps its address — degraded, not failed — and
// is ready again as soon as the server answers.
func TestSupervisorKeepsTheConnectorWhileTheServerIsDown(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.checker.setLocalErr(errors.New("connection refused"))
	h.enable(t, "rev-1")

	st := h.waitState(t, StateDegraded, 30*time.Second)
	if st.URL == "" || !strings.Contains(st.Message, "the server does not answer") {
		t.Fatalf("status = %+v, want the address kept and the server named as the problem", st)
	}
	if st.Failures != 0 {
		t.Fatalf("a server that is down counted as a connector failure: %+v", st)
	}
	pid := h.connectorPID(t)

	h.checker.setLocalErr(nil)
	ready := h.waitState(t, StateReady, 30*time.Second)
	if ready.URL != st.URL || h.connectorPID(t) != pid {
		t.Fatalf("the connector was replaced while the server was away: %+v", ready)
	}

	// And the other way round: the server goes away under a ready entry point.
	h.checker.setLocalErr(errors.New("connection refused"))
	h.waitState(t, StateDegraded, 30*time.Second)
	if h.connectorPID(t) != pid {
		t.Fatal("the connector was replaced when the server went away")
	}
}

// TestServiceExitsOnceTheEntryPointIsDisabled: the connector service exists
// only to host an entry point. Disabled, it stops the connector and exits —
// which is what `expose down` waits for, and why its systemd unit restarts it
// on failure only.
func TestServiceExitsOnceTheEntryPointIsDisabled(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	// The harness started its supervisor with nothing configured; the service
	// is started the way `up` starts it, after the request is written.
	h.cancel()
	<-h.done
	h.enable(t, "rev-1")
	h.sup = NewSupervisor(h.store, h.sup.deps, h.sup.log)
	h.sup.SetInterval(20 * time.Millisecond)
	h.sup.SetListenAddr("0.0.0.0:8800")
	h.sup.exitWhenDisabled = true
	h.start(t)
	h.waitState(t, StateReady, 30*time.Second)
	pid := h.connectorPID(t)

	if err := h.store.WriteConfig(&Config{Revision: "rev-2", Provider: ProviderQuick, Desired: DesiredDisabled}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.done:
	case <-time.After(40 * time.Second):
		t.Fatal("the service kept running with nothing to host")
	}
	if alive(pid) {
		t.Fatal("the service exited and left the connector running")
	}
	if h.store.SupervisorHeld() {
		t.Fatal("the hosting lock outlived the service")
	}
}

// TestServiceWithNothingConfiguredExits: started without a request (or after
// `rm`), there is nothing to do.
func TestServiceWithNothingConfiguredExits(t *testing.T) {
	store := NewStore(t.TempDir())
	done := make(chan error, 1)
	go func() { done <- RunService(t.Context(), store, nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunService: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the service kept running with nothing configured")
	}
}

// TestRunServiceRefusesASecondService: one data dir, one connector service.
func TestRunServiceRefusesASecondService(t *testing.T) {
	store := NewStore(t.TempDir())
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := RunService(t.Context(), store, nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("RunService = %v, want a refusal", err)
	}
}

func TestResolveOrigin(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	if _, _, err := ResolveOrigin(store, ""); err == nil || !strings.Contains(err.Error(), "--origin") {
		t.Fatalf("with nothing recorded: %v, want advice to pass --origin", err)
	}
	if err := RecordServerOrigin(dir, "0.0.0.0:8800"); err != nil {
		t.Fatal(err)
	}
	if origin, how, err := ResolveOrigin(store, ""); err != nil || origin != "http://127.0.0.1:8800" || how != "recorded by the server" {
		t.Fatalf("recorded wildcard = %q (%s) %v, want loopback", origin, how, err)
	}
	if err := RecordServerOrigin(dir, "[::]:8800"); err != nil {
		t.Fatal(err)
	}
	if origin, _, _ := ResolveOrigin(store, ""); origin != "http://[::1]:8800" {
		t.Fatalf("recorded IPv6 wildcard = %q", origin)
	}
	// An explicit origin wins, and has to be a URL.
	if origin, how, err := ResolveOrigin(store, "http://10.0.0.5:8800/"); err != nil || origin != "http://10.0.0.5:8800" || how != "--origin" {
		t.Fatalf("explicit = %q (%s) %v", origin, how, err)
	}
	if _, _, err := ResolveOrigin(store, "10.0.0.5:8800"); err == nil {
		t.Fatal("an origin without a scheme was accepted")
	}
	// The record is readable by the account the CLI runs as.
	info, err := os.Stat(store.ServerOriginPath())
	if err != nil || info.Mode().Perm()&0o044 == 0 {
		t.Fatalf("server-origin mode = %v (%v)", info.Mode(), err)
	}
}

func TestRenderServiceUnit(t *testing.T) {
	unit, err := RenderServiceUnit("/opt/gpu works/gpuworks", "/opt/gpuworks/100%/data", "gpuworks")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"\nUser=gpuworks\n",
		`ExecStart="/opt/gpu works/gpuworks" expose run --data /opt/gpuworks/100%%/data` + "\n",
		// It exits with success once disabled; only a failure restarts it.
		"\nRestart=on-failure\n",
		"\nWantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, unit)
		}
	}
	if root, _ := RenderServiceUnit("/usr/bin/gpuworks", "/data", ""); strings.Contains(root, "User=") {
		t.Errorf("a unit for the current account names a user:\n%s", root)
	}
	if _, err := RenderServiceUnit("/usr/bin/gpu\"works", "/data", ""); err == nil {
		t.Error("a path systemd cannot quote was accepted")
	}
}

func TestServiceAccountIsNilForTheCurrentUser(t *testing.T) {
	dir := t.TempDir()
	account, err := ServiceAccount(dir)
	if err != nil || account != nil {
		t.Fatalf("ServiceAccount(own dir) = %v, %v; want nil (run as this user)", account, err)
	}
	if _, err := ServiceAccount(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("a missing data dir was accepted")
	}
}
