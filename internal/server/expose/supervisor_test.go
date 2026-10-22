package expose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The supervisor tests run the real process machinery — the helper, the
// liveness pipe, Wait — against a fake cloudflared on PATH, which is how the
// plan asks for it. Only the network is faked: the health checker decides what
// "reachable" means, because readiness is the judgement this feature must never
// get wrong.

// A connector that reports a URL derived from its own pid, so a restart is
// visibly a different address (exactly what a quick tunnel does). It also
// records every start, so a replacement is observable as a new pid rather than
// only as a count.
const supervisorConnectorScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
echo "$$" >> "$FAKE_CF_STARTS"
echo "$$" > "$FAKE_CF_PIDFILE"
echo 'INF |  Your quick Tunnel has been created! Visit it at:  |'
echo "INF |  https://tunnel-$$.trycloudflare.com  |"
echo 'INF Registered tunnel connection connIndex=0 event=0 ip=198.41.192.1 location=lax99 protocol=quic'
while true; do sleep 1; done
`

// A connector that dies immediately, for the failure paths.
const supervisorCrashScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo 'cloudflared version 2026.9.1 (fake)'
	exit 0
fi
echo start >> "$FAKE_CF_STARTS"
echo 'ERR failed to dial to edge with quic: no recent network activity' >&2
exit 1
`

// waitFor polls a condition: these tests observe real child processes, whose
// timing is the kernel's business, not the test's.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type fakeChecker struct {
	mu        sync.Mutex
	publicErr error
	localErr  error
	public    []string
}

func (f *fakeChecker) CheckPublic(_ context.Context, url string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.public = append(f.public, url)
	return f.publicErr
}

func (f *fakeChecker) CheckLocal(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.localErr
}

func (f *fakeChecker) setPublicErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.publicErr = err
}

func (f *fakeChecker) checkedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.public...)
}

// syncBuffer collects the supervisor's own account of what it did. A failing
// supervisor test is nearly always a timing judgement, and the decisions behind
// it are only in this log.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// supervisorHarness is one supervisor under test, with the fake connector on
// PATH and its pid file watched so a test can see the real child.
type supervisorHarness struct {
	store    *Store
	sup      *Supervisor
	checker  *fakeChecker
	pidFile  string
	startsAt string
	cancel   context.CancelFunc
	done     chan struct{}
	log      *syncBuffer
}

func newSupervisorHarness(t *testing.T, script string) *supervisorHarness {
	t.Helper()
	dir := t.TempDir()
	writeFakeConnector(t, dir, script)
	// The provider finds cloudflared through PATH, the way the plan intends for
	// tests: no download, no injection of a private hook.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CF_PIDFILE", filepath.Join(dir, "connector.pid"))
	t.Setenv("FAKE_CF_STARTS", filepath.Join(dir, "starts"))

	store := NewStore(t.TempDir())
	checker := &fakeChecker{}
	deps := NewDeps()
	deps.Prober = checker
	deps.Helper = testHelper(t)
	logs := &syncBuffer{}
	sup := NewSupervisor(store, deps, slog.New(slog.NewTextHandler(logs, nil)))
	sup.SetInterval(20 * time.Millisecond)
	sup.SetStartTimeout(10 * time.Second)
	sup.SetProbeInterval(50 * time.Millisecond)
	sup.SetProbeWindow(300 * time.Millisecond)
	sup.SetStableWindow(50 * time.Millisecond)
	sup.SetListenAddr("0.0.0.0:8800")

	h := &supervisorHarness{
		store: store, sup: sup, checker: checker, log: logs,
		pidFile:  filepath.Join(dir, "connector.pid"),
		startsAt: filepath.Join(dir, "starts"),
	}
	// Registered before start's cleanup, so it runs after the supervisor has
	// been stopped: a failure then carries both accounts — what the supervisor
	// decided, and what the connector and its helper printed. The helper writes
	// its own stop messages into the connector's log.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("supervisor log:\n%s", logs.String())
		if raw, err := os.ReadFile(store.LogPath()); err == nil {
			t.Logf("connector log:\n%s", raw)
		}
	})
	h.start(t)
	return h
}

func (h *supervisorHarness) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		defer close(h.done)
		h.sup.Run(ctx)
	}()
	t.Cleanup(func() {
		h.cancel()
		select {
		case <-h.done:
		case <-time.After(30 * time.Second):
			t.Error("the supervisor did not stop")
		}
	})
}

// enable writes an enabled configuration for the quick provider.
func (h *supervisorHarness) enable(t *testing.T, revision string) {
	t.Helper()
	if err := h.store.WriteConfig(&Config{Revision: revision, Provider: ProviderQuick, Desired: DesiredEnabled}); err != nil {
		t.Fatal(err)
	}
}

func (h *supervisorHarness) waitState(t *testing.T, state string, timeout time.Duration) *Status {
	return h.waitStateFor(t, "", state, timeout)
}

// waitStateFor waits for a state *of a given revision*: a status left over
// from the previous revision is not the answer to a new request.
func (h *supervisorHarness) waitStateFor(t *testing.T, revision, state string, timeout time.Duration) *Status {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last *Status
	for time.Now().Before(deadline) {
		st, _ := h.store.ReadStatus()
		last = st
		if st != nil && st.State == state && (revision == "" || st.Revision == revision) {
			return st
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state %q of revision %q was not reached; last status: %+v", state, revision, last)
	return nil
}

// startCount is how many times the fake connector has been started.
func (h *supervisorHarness) startCount(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(h.startsAt)
	if err != nil {
		return 0
	}
	return strings.Count(string(raw), "start")
}

// startPIDs is every pid the fake connector has recorded, oldest first.
func (h *supervisorHarness) startPIDs(t *testing.T) []int {
	t.Helper()
	raw, err := os.ReadFile(h.startsAt)
	if err != nil {
		return nil
	}
	var pids []int
	for _, line := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(line); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitForConnectorOtherThan waits for the pid file to name a connector other
// than the one given: the supervisor has replaced it.
func (h *supervisorHarness) waitForConnectorOtherThan(t *testing.T, old int, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pid := h.connectorPID(t); pid != old {
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the connector was not replaced; pid file still names %d", old)
	return 0
}

func (h *supervisorHarness) connectorPID(t *testing.T) int {
	t.Helper()
	return waitForPidFile(t, h.pidFile)
}

func TestSupervisorReachesReadyAndStopsOnShutdown(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")

	st := h.waitState(t, StateReady, 30*time.Second)
	if !strings.HasPrefix(st.URL, "https://tunnel-") {
		t.Fatalf("url = %q, want the address the connector printed", st.URL)
	}
	if st.Revision != "rev-1" || st.Provider != ProviderQuick {
		t.Errorf("status = %+v, want this revision and provider", st)
	}
	if st.LocalOrigin != "http://127.0.0.1:8800" {
		t.Errorf("local origin = %q, want the bound address normalised to loopback", st.LocalOrigin)
	}
	if st.BinaryVersion != "2026.9.1" {
		t.Errorf("connector version = %q", st.BinaryVersion)
	}
	if st.Edge != "lax99" || st.Protocol != "quic" {
		t.Errorf("edge/protocol = %q/%q, want what the connector reported", st.Edge, st.Protocol)
	}
	if urls := h.checker.checkedURLs(); len(urls) == 0 || urls[0] != st.URL {
		t.Errorf("the public probe saw %v, want the published address", urls)
	}
	pid := h.connectorPID(t)

	// Shutting the server down closes the entry point: an address pointing at a
	// stopped server is worse than one that has to be re-established.
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the supervisor did not stop")
	}
	// Generous on purpose: a connector that ignores SIGTERM is killed after the
	// helper's grace (10s) plus its kill wait (5s). What is asserted is that it
	// goes away, not how quickly.
	waitFor(t, 40*time.Second, "the connector to exit", func() bool { return !alive(pid) })
	st, _ = h.store.ReadStatus()
	if st == nil || st.State != StateDisabled || st.URL != "" {
		t.Fatalf("status after shutdown = %+v, want disabled with no address", st)
	}
}

// TestSupervisorKeepsAWorkingConnector: the helper's liveness is asked about on
// every pass, and a false negative there would mint a new address on every tick
// — the operator would watch the entry point move for no reason at all.
func TestSupervisorKeepsAWorkingConnector(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")

	st := h.waitState(t, StateReady, 30*time.Second)
	pid := h.connectorPID(t)
	// Many ticks of a 20ms interval.
	time.Sleep(300 * time.Millisecond)

	if now := h.connectorPID(t); now != pid {
		t.Fatalf("the connector was replaced (%d -> %d) while nothing was wrong", pid, now)
	}
	if got, _ := h.store.ReadStatus(); got == nil || got.State != StateReady || got.URL != st.URL {
		t.Fatalf("status = %+v, want the same working address %q", got, st.URL)
	}
	if pids := h.startPIDs(t); len(pids) != 1 {
		t.Fatalf("the connector was started %d times, want once: %v", len(pids), pids)
	}
}

// TestSupervisorReplacesAConnectorWhoseHelperDied: the helper is what turns a
// dead server into a stopped connector, so a connector that lost its helper is
// unmanaged — the moment the server is killed it would keep serving that
// server's address from the public internet. The supervisor has to notice, take
// the unmanaged connector down, and start again under a helper that is there.
func TestSupervisorReplacesAConnectorWhoseHelperDied(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.sup.SetBackoff([]time.Duration{time.Millisecond})
	h.enable(t, "rev-1")

	h.waitState(t, StateReady, 30*time.Second)
	first := h.connectorPID(t)
	child := h.sup.currentChild()
	if child == nil {
		t.Fatal("the supervisor should own the connector")
	}

	// The helper goes and the connector stays — an OOM kill, a stray signal.
	if err := syscall.Kill(child.PID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	second := h.waitForConnectorOtherThan(t, first, 30*time.Second)
	if alive(first) {
		t.Fatalf("connector %d was left running with no helper to stop it", first)
	}
	if !alive(second) {
		t.Fatalf("the replacement connector %d is not running", second)
	}
	// The replacement publishes its own address, and the observation is about
	// that one: a stale URL pointing at a connector that was taken down is
	// exactly what must not survive here.
	st := h.waitState(t, StateReady, 30*time.Second)
	if want := fmt.Sprintf("https://tunnel-%d.trycloudflare.com", second); st.URL != want {
		t.Fatalf("url = %q, want the replacement's address %q", st.URL, want)
	}
}

func TestSupervisorGivesUpAfterRepeatedFailures(t *testing.T) {
	h := newSupervisorHarness(t, supervisorCrashScript)
	h.sup.SetBackoff([]time.Duration{time.Millisecond, time.Millisecond})
	h.enable(t, "rev-1")

	st := h.waitState(t, StateFailed, 30*time.Second)
	if st.LastError == "" {
		t.Fatalf("a failed attempt must say why: %+v", st)
	}
	// Wait for the give-up message, then check that attempts really stop.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, _ = h.store.ReadStatus()
		if st != nil && strings.Contains(st.Message, "gave up") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || !strings.Contains(st.Message, "gave up") {
		t.Fatalf("the supervisor never gave up: %+v", st)
	}
	// What must stop is starting connectors. An attempt that was already in
	// flight when the decision was made still finishes (and its network ladder
	// can start one more), so the honest assertion is that starting *settles*:
	// a supervisor that keeps launching would never stop moving. A fixed sleep
	// here only measures how loaded the machine is.
	settled := h.startCount(t)
	quiet := 0
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && quiet < 3 {
		time.Sleep(200 * time.Millisecond)
		if now := h.startCount(t); now != settled {
			settled = now
			quiet = 0
			continue
		}
		quiet++
	}
	if quiet < 3 {
		t.Fatalf("the supervisor never stopped starting connectors: %d and counting", settled)
	}
}

func TestSupervisorLeavesDisabledAlone(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	if err := h.store.WriteConfig(&Config{Revision: "rev-off", Provider: ProviderQuick, Desired: DesiredDisabled}); err != nil {
		t.Fatal(err)
	}
	// Wait until the supervisor has acted on this configuration at all — a
	// fixed sleep is a race against however loaded the machine is — and then
	// assert that acting on it started nothing.
	st := h.waitStateFor(t, "rev-off", StateDisabled, 30*time.Second)
	if st.Revision != "rev-off" {
		t.Fatalf("status = %+v, want the observation of the disabled revision", st)
	}
	if _, err := os.Stat(h.pidFile); err == nil {
		t.Fatal("a disabled configuration must not start a connector")
	}
}

// TestSupervisorRestartsAfterCrash: the connector dying is not the end of the
// entry point, and the replacement is a new address.
func TestSupervisorRestartsAfterCrash(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")
	first := h.waitState(t, StateReady, 30*time.Second)
	firstPID := h.connectorPID(t)

	// The connector is a member of the helper's process group, not a leader of
	// its own, so its pid is what names it: a group signal would have nothing
	// to address.
	if err := syscall.Kill(firstPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	var second *Status
	for time.Now().Before(deadline) {
		st, _ := h.store.ReadStatus()
		if st != nil && st.State == StateReady && st.URL != first.URL {
			second = st
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if second == nil {
		st, _ := h.store.ReadStatus()
		t.Fatalf("the entry point did not come back at a new address: %+v", st)
	}
	// The failure count is cleared only after the entry point has held for the
	// stable window, so it is cleared asynchronously by a later probe — an
	// immediate crash then restart must not look like a recovery.
	waitFor(t, 20*time.Second, "the failure count to clear once the window passed", func() bool {
		st, _ := h.store.ReadStatus()
		return st != nil && st.State == StateReady && st.Failures == 0
	})
}

// TestSupervisorInvalidatesAStaleReady is the rule the CLI depends on: a ready
// snapshot from a previous server is not an address, and whoever reads it must
// not be told otherwise.
func TestSupervisorInvalidatesAStaleReady(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.WriteStatus(&Status{
		Revision: "rev-1", Provider: ProviderQuick, State: StateReady,
		URL: "https://stale.trycloudflare.com",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteConfig(&Config{Revision: "rev-1", Provider: ProviderQuick, Desired: DesiredDisabled}); err != nil {
		t.Fatal(err)
	}
	checker := &fakeChecker{}
	deps := NewDeps()
	deps.Prober = checker
	sup := NewSupervisor(store, deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.SetInterval(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(ctx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := store.ReadStatus()
		if st != nil && st.URL == "" && st.State != StateReady {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := store.ReadStatus()
	t.Fatalf("the stale address survived: %+v", st)
}

// TestSupervisorRevisionChangeReplacesTheConnector: switching provider or
// asking again is an explicit act, and the old connector goes first.
func TestSupervisorRevisionChangeReplacesTheConnector(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")
	h.waitState(t, StateReady, 30*time.Second)
	firstPID := h.connectorPID(t)

	h.enable(t, "rev-2")
	h.waitStateFor(t, "rev-2", StateReady, 30*time.Second)
	secondPID := h.connectorPID(t)
	if secondPID == firstPID {
		t.Fatalf("the connector was not replaced (pid %d)", firstPID)
	}
	waitFor(t, 10*time.Second, "the previous connector to exit", func() bool { return !alive(firstPID) })
}

// TestSupervisorSecondServerDoesNotHost: two servers on one data dir must not
// each publish an entry point.
func TestSupervisorSecondServerDoesNotHost(t *testing.T) {
	store := NewStore(t.TempDir())
	release, err := store.SupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	checker := &fakeChecker{}
	deps := NewDeps()
	deps.Prober = checker
	sup := NewSupervisor(store, deps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.SetInterval(20 * time.Millisecond)
	done := make(chan struct{})
	go func() { defer close(done); sup.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a supervisor without the hosting lock must return instead of hosting")
	}
}

// TestSupervisorDegradedWhenTheAddressStopsAnswering: a connector that is alive
// but unreachable is degraded, not failed — the address is kept and recovered.
func TestSupervisorDegradedWhenTheAddressStopsAnswering(t *testing.T) {
	h := newSupervisorHarness(t, supervisorConnectorScript)
	h.enable(t, "rev-1")
	h.waitStateFor(t, "rev-1", StateReady, 30*time.Second)

	h.checker.setPublicErr(errors.New("dial tcp: lookup tunnel-x.trycloudflare.com: no such host"))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := h.store.ReadStatus(); st != nil && st.State == StateDegraded {
			if st.URL == "" {
				t.Fatal("degraded must keep the address it already published")
			}
			h.checker.setPublicErr(nil)
			recovered := time.Now().Add(30 * time.Second)
			for time.Now().Before(recovered) {
				if st, _ := h.store.ReadStatus(); st != nil && st.State == StateReady {
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			t.Fatal("the entry point did not recover to ready once it answered again")
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := h.store.ReadStatus()
	t.Fatalf("status never went degraded: %+v", st)
}
