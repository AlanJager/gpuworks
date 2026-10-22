package expose

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Everything in this file runs real processes. The contract under test — a
// server killed with SIGKILL must not leave a public connector serving a dead
// origin — cannot be proven with a stub, and the plan asks for platform
// evidence on exactly this path.

// TestConnectorHostHelperProcess is the helper entry point inside the test
// binary. Production reaches the same code through `gpuworks expose
// connector-host`.
func TestConnectorHostHelperProcess(t *testing.T) {
	if os.Getenv(HostSpecEnv) == "" {
		t.Skip("entry point for the connector helper, not a test on its own")
	}
	os.Exit(HostMain(context.Background(), os.Stdin))
}

// The fake connector reports its pid, prints what a real quick tunnel prints,
// and stays alive until it is killed.
const fakeConnectorScript = `#!/bin/sh
if [ -n "$FAKE_CF_PIDFILE" ]; then echo $$ > "$FAKE_CF_PIDFILE"; fi
echo 'INF |  Your quick Tunnel has been created! Visit it at:  |'
echo 'INF |  https://fake-connector.trycloudflare.com  |'
echo 'INF Registered tunnel connection connIndex=0 event=0 ip=198.41.192.1 location=lax99 protocol=quic'
while true; do sleep 1; done
`

// parentKillMarker switches a test binary into "act as the server" mode.
const parentKillMarker = "GPUWORKS_EXPOSE_TEST_PARENT"

func testHelper(t *testing.T) HelperCommand {
	t.Helper()
	return HelperCommand{Path: os.Args[0], Args: []string{"-test.run=TestConnectorHostHelperProcess"}}
}

func writeFakeConnector(t *testing.T, dir, script string) string {
	t.Helper()
	path := filepath.Join(dir, "cloudflared")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func childSpec(fake, pidFile string) Spec {
	return Spec{
		Binary: fake,
		Args:   []string{"tunnel", "--url", "http://127.0.0.1:8800", "--no-autoupdate"},
		Env:    append(os.Environ(), "FAKE_CF_PIDFILE="+pidFile),
	}
}

func waitForPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the connector never reported a pid in %s", path)
	return 0
}

// alive reports whether the process is still running. A zombie counts as gone:
// after a process dies its pid stays in the table until it is reaped, a
// container's PID 1 does not reap grandchildren, and a zombie serves no
// traffic — reading it as running would turn "the connector is gone" into an
// assertion no machine can satisfy. The L2 harness draws the same line for the
// same reason.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// ps exits 1 when no process matches — it died between the signal above
		// and this call, which is the answer, not a reason to assume it lives.
		// Only a ps that cannot run at all leaves the pid answering kill as the
		// evidence. (Starting ps takes milliseconds; the reaper does not.)
		var exit *exec.ExitError
		return !errors.As(err, &exit)
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func TestChildStopTakesTheConnectorDown(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeConnector(t, dir, fakeConnectorScript)
	pidFile := filepath.Join(dir, "connector.pid")

	var logs bytes.Buffer
	child, err := StartChild(testHelper(t), childSpec(fake, pidFile), &logs)
	if err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	connectorPID := waitForPidFile(t, pidFile)
	if !alive(connectorPID) {
		t.Fatal("the connector should be running")
	}
	if err := child.Stop(20 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if alive(connectorPID) {
		t.Fatalf("connector pid %d survived Stop", connectorPID)
	}
	if !strings.Contains(logs.String(), "Registered tunnel connection") {
		t.Errorf("the connector's output must reach the server's writer, got:\n%s", logs.String())
	}
	if !child.Exited() {
		t.Error("Exited must report true after Stop")
	}
}

// TestChildTakesDownAConnectorWhoseHelperDied: the helper is the only process
// that turns "the server is gone" into "the connector stops", so a connector
// must not be able to outlive it. The helper is killed outright here — an OOM
// kill, a stray signal — and what it left behind must still go away.
//
// Nothing reports the helper's death: Wait is blocked on the output pipe, which
// the surviving connector holds open, so the lease has to be asked directly.
func TestChildTakesDownAConnectorWhoseHelperDied(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeConnector(t, dir, fakeConnectorScript)
	pidFile := filepath.Join(dir, "connector.pid")

	var logs bytes.Buffer
	child, err := StartChild(testHelper(t), childSpec(fake, pidFile), &logs)
	if err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	connectorPID := waitForPidFile(t, pidFile)
	if !child.HelperAlive() {
		t.Fatal("a helper that is running must not look gone")
	}
	if err := syscall.Kill(child.PID(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The kernel tears the helper down asynchronously, so the lease is asked
	// until it answers that nobody is holding it any more.
	waitFor(t, 10*time.Second, "the killed helper to look gone", func() bool { return !child.HelperAlive() })
	if child.Exited() {
		t.Error("Wait should still be blocked by the connector holding the output pipe")
	}
	if !alive(connectorPID) {
		t.Fatal("the connector should still be running after its helper was killed")
	}

	start := time.Now()
	if err := child.Stop(30 * time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The grace belongs to a helper that can still act on it; with none left,
	// waiting it out would only make the server's shutdown pay for a process
	// that no longer exists.
	if elapsed := time.Since(start); elapsed > orphanedConnectorGrace {
		t.Errorf("Stop took %v with no helper left, want under %v", elapsed, orphanedConnectorGrace)
	}
	if alive(connectorPID) {
		t.Fatalf("connector pid %d survived Stop after its helper was killed (%s)", connectorPID, psInfo(t, connectorPID))
	}
}

// psInfo describes one process the way a failure needs to see it: identity,
// parent, process group and state. A connector that "survived" is either
// running, a zombie nobody reaped, or a pid that now belongs to something else,
// and those need different answers.
func psInfo(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "pid=,ppid=,pgid=,stat=,args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "no such process (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

// TestChildReportsConnectorExitCode: the server needs to tell a crash from a
// clean stop, which is what the helper's exit status carries.
func TestChildReportsConnectorExitCode(t *testing.T) {
	dir := t.TempDir()
	fake := writeFakeConnector(t, dir, "#!/bin/sh\nexit 7\n")
	child, err := StartChild(testHelper(t), childSpec(fake, filepath.Join(dir, "unused.pid")), io.Discard)
	if err != nil {
		t.Fatalf("StartChild: %v", err)
	}
	err = child.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("Wait = %v, want the connector's exit code 7", err)
	}
	if err := child.Stop(time.Second); err != nil {
		t.Errorf("Stop after the connector already exited should be clean, got %v", err)
	}
}

// TestChildLeavesNoConnectorWhenTheServerIsKilled is the evidence the plan
// asks for: SIGKILL the server, with no shutdown path of its own, and the
// connector must still go away.
func TestChildLeavesNoConnectorWhenTheServerIsKilled(t *testing.T) {
	if os.Getenv(parentKillMarker) != "" {
		parentKillMain() // never returns
	}
	dir := t.TempDir()
	fake := writeFakeConnector(t, dir, fakeConnectorScript)
	t.Cleanup(func() { killByPath(t, fake) }) // belt and braces for a failed assertion

	cmd := exec.Command(os.Args[0], "-test.run=TestChildLeavesNoConnectorWhenTheServerIsKilled")
	cmd.Env = append(os.Environ(), parentKillMarker+"=1", "EXPOSE_TEST_DIR="+dir)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	serverPID := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(serverPID, syscall.SIGKILL) })

	connectorPID := waitForPidFile(t, filepath.Join(dir, "connector.pid"))
	// The server dies the way nothing can prepare for.
	if err := syscall.Kill(serverPID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(connectorPID) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("connector pid %d survived the server's SIGKILL", connectorPID)
}

// TestChildStartupRaceLeavesNoOrphan kills the server at varying points around
// startup, including before the helper has read its spec — the race the plan
// calls out.
func TestChildStartupRaceLeavesNoOrphan(t *testing.T) {
	if os.Getenv(parentKillMarker) != "" {
		parentKillMain()
	}
	for i := 0; i < 10; i++ {
		dir := t.TempDir()
		fake := writeFakeConnector(t, dir, fakeConnectorScript)
		cmd := exec.Command(os.Args[0], "-test.run=TestChildStartupRaceLeavesNoOrphan")
		cmd.Env = append(os.Environ(), parentKillMarker+"=1", "EXPOSE_TEST_DIR="+dir)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Duration(i) * 20 * time.Millisecond)
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()

		// Whatever was started must be gone. Nothing here uses the pid file:
		// the point is to catch a connector that never got that far.
		deadline := time.Now().Add(30 * time.Second)
		for {
			if !aliveByPath(t, fake) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("iteration %d: a connector survived the server's SIGKILL", i)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// parentKillMain is the body of the "server" subprocess: start a connector
// through the helper and then idle. It never stops the child on purpose — the
// outer test kills this process without warning, which is the entire scenario.
func parentKillMain() {
	dir := os.Getenv("EXPOSE_TEST_DIR")
	fake := filepath.Join(dir, "cloudflared")
	// An io.Writer, not an *os.File: os/exec then gives the helper a pipe to
	// this process, exactly as the server does. Handing it a file instead would
	// keep the helper's stdout writable after we are killed, which is not the
	// situation being tested.
	var sink discardSink
	if _, err := StartChild(HelperCommand{
		Path: os.Args[0],
		Args: []string{"-test.run=TestConnectorHostHelperProcess"},
	}, Spec{
		Binary: fake,
		Args:   []string{"tunnel", "--url", "http://127.0.0.1:8800", "--no-autoupdate"},
		Env:    append(os.Environ(), "FAKE_CF_PIDFILE="+filepath.Join(dir, "connector.pid")),
	}, sink); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	select {} // wait to be killed
}

// aliveByPath reports whether any process still runs the fake connector at
// path. The fake's own command line carries the path; the test process's does
// not, so this cannot match itself.
func aliveByPath(t *testing.T, path string) bool {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep is not available to check for leftover processes")
	}
	out, err := exec.Command("pgrep", "-f", path).Output()
	if err != nil {
		return false // exit status 1 means "no match"
	}
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil && pid != os.Getpid() {
			return true
		}
	}
	return false
}

func killByPath(t *testing.T, path string) {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", path).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil && pid != os.Getpid() {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// discardSink is an io.Writer the helper's output is copied into. It exists so
// the parent passes os/exec a writer rather than a file descriptor, which is
// what production does and what makes the broken-pipe case real.
type discardSink struct{}

func (discardSink) Write(p []byte) (int, error) { return len(p), nil }
