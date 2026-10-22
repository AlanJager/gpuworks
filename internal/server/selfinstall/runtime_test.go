package selfinstall

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunnerRunsCommands(t *testing.T) {
	runner := ExecRunner{Euid: func() int { return 0 }, Stderr: io.Discard}
	ctx := context.Background()
	if err := runner.Run(ctx, Command{Path: "true"}); err != nil {
		t.Fatalf("true: %v", err)
	}
	err := runner.Run(ctx, Command{Path: "false"})
	if err == nil {
		t.Fatal("a failing command was reported as success")
	}
	if !strings.Contains(err.Error(), "false") {
		t.Fatalf("the error does not name the command: %v", err)
	}
	out, err := runner.Output(ctx, Command{Path: "sh", Args: []string{"-c", "echo hello"}})
	if err != nil || out != "hello" {
		t.Fatalf("Output = %q, %v", out, err)
	}
	if err := runner.Run(ctx, Command{Path: "definitely-not-a-real-command-4f2a"}); err == nil {
		t.Fatal("a missing binary was reported as success")
	}
	if err := runner.Run(ctx, Command{}); err == nil {
		t.Fatal("an empty command was reported as success")
	}
}

func TestExecRunnerEscalatesThroughSudo(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "args")
	// A stand-in on PATH that records how it was invoked, so the escalation is
	// observed directly instead of through whatever sudo the host happens to
	// have installed.
	writeFile(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nprintf '%s\\n' \"$*\" > "+record+"\n", 0o755)
	t.Setenv("PATH", dir)
	// The escalated command has to exist on the host running the suite: /bin/true
	// is Linux-only, so the fixture is a script this test owns.
	noop := filepath.Join(dir, "noop")
	writeFile(t, noop, "#!/bin/sh\nexit 0\n", 0o755)

	runner := ExecRunner{Euid: func() int { return 1000 }, Stderr: io.Discard}
	if err := runner.Run(context.Background(), Command{Path: noop, Args: []string{"--flag"}, Sudo: true}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Plain sudo: it prompts on the operator's terminal when there is no
	// cached ticket and fails on its own when there is no terminal.
	if got := strings.TrimSpace(readFile(t, record)); got != noop+" --flag" {
		t.Fatalf("sudo was invoked as %q", got)
	}

	// A root process runs the command itself; there is nothing to escalate to.
	if err := os.Remove(record); err != nil {
		t.Fatalf("remove: %v", err)
	}
	root := ExecRunner{Euid: func() int { return 0 }, Stderr: io.Discard}
	if err := root.Run(context.Background(), Command{Path: noop, Args: []string{"x"}, Sudo: true}); err != nil {
		t.Fatalf("a root runner failed: %v", err)
	}
	if fileExists(record) {
		t.Fatal("a root runner escalated anyway")
	}
}

func TestExecRunnerPassesStdinAndCapturesStderr(t *testing.T) {
	var stderr, stdout strings.Builder
	runner := ExecRunner{Euid: func() int { return 0 }, Stderr: &stderr}
	err := runner.Run(context.Background(), Command{
		Path: "sh", Args: []string{"-c", "cat; echo oops >&2"},
		Stdin: strings.NewReader("from stdin"), Stdout: &stdout,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout.String() != "from stdin" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "oops") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestExecRunnerRunsInTheRequestedDirectory(t *testing.T) {
	dir := t.TempDir()
	runner := ExecRunner{Euid: func() int { return 0 }, Stderr: io.Discard}
	out, err := runner.Output(context.Background(), Command{Path: "pwd", Dir: dir})
	if err != nil {
		t.Fatalf("pwd: %v", err)
	}
	// macOS reports /private/var for /var, so compare the resolved paths.
	if resolve(t, out) != resolve(t, dir) {
		t.Fatalf("working directory = %q, want %q", out, dir)
	}
}

func TestPidFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.pid")
	if got := readPID(path); got != 0 {
		t.Fatalf("a missing pid file reads as %d", got)
	}
	if err := writePID(path, 4242); err != nil {
		t.Fatalf("writePID: %v", err)
	}
	if got := readPID(path); got != 4242 {
		t.Fatalf("pid = %d", got)
	}
	// A file another tool mangled must read as "nothing of ours is running",
	// never as a pid to signal.
	for _, content := range []string{"", "\n", "not a number", "-1", "0", "12 34"} {
		writeFile(t, path, content, 0o644)
		if got := readPID(path); got != 0 {
			t.Fatalf("pid file %q reads as %d", content, got)
		}
	}
}

func TestIsOurServerChecksWhatThePidRuns(t *testing.T) {
	h := newHarness(t, t.TempDir())
	ctx := context.Background()
	sleeper, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep is not on PATH: %v", err)
	}
	pid := startSleeper(t)
	// Platforms without /proc answer through ps, which the runner supplies.
	h.run.output["ps -p "+strconv.Itoa(pid)+" -o command="] = sleeper + " 30"

	if !isOurServer(ctx, h.run, pid, sleeper) {
		t.Fatalf("pid %d running %s was not recognised", pid, sleeper)
	}
	if isOurServer(ctx, h.run, pid, "/opt/gpuworks/server/gpuworks") {
		t.Fatal("a process running another program was claimed as ours")
	}
	if isOurServer(ctx, h.run, 0, sleeper) || isOurServer(ctx, h.run, 1<<30, sleeper) {
		t.Fatal("a pid that names no live process was claimed as ours")
	}
	// A pid whose command line cannot be read is not ours either: the check must
	// fail closed rather than signal a stranger.
	if argv := cmdline(ctx, h.run, 1<<30); len(argv) != 0 {
		t.Fatalf("cmdline of a dead pid = %v", argv)
	}
}

func TestProcessHelpers(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("the running process is not alive")
	}
	if processAlive(0) || processAlive(-1) || processAlive(1<<30) {
		t.Fatal("a pid that cannot name a live process was reported alive")
	}
	free, err := freeBytes(t.TempDir())
	if err != nil {
		t.Fatalf("freeBytes: %v", err)
	}
	if free == 0 {
		t.Fatal("freeBytes reported no space")
	}
	if _, err := freeBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("freeBytes accepted a path that does not exist")
	}
	if err := signalProcess(1<<30, "TERM"); err == nil {
		t.Fatal("signalling a pid that cannot exist was reported as success")
	}
}

func TestSignalProcessStopsAChild(t *testing.T) {
	pid := startSleeper(t)
	if err := signalProcess(pid, "TERM"); err != nil {
		t.Fatalf("signalProcess: %v", err)
	}
	if !waitGone(pid, 5*time.Second) {
		t.Fatalf("pid %d survived SIGTERM", pid)
	}
}

func TestWaitForExitNoticesADeadProcess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.deps.StopTimeout = time.Second
	if !waitForExit(context.Background(), h.deps, 1<<30) {
		t.Fatal("a pid that names no process did not count as exited")
	}
}

func TestWaitForExitEscalatesToSigkill(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.deps.StopTimeout = time.Second
	// A real sleep here so the kernel has time to reap what it kills; the wait
	// itself is still counted in attempts.
	h.deps.Sleep = func(_ context.Context, d time.Duration) { time.Sleep(d / 10) }
	pid := startSleeperIgnoringTERM(t)

	waitForExit(context.Background(), h.deps, pid)
	// SIGTERM was ignored, so only the escalation can have ended it.
	if !waitGone(pid, 5*time.Second) {
		t.Fatalf("pid %d survived the stop timeout", pid)
	}
}

func TestStartDetachedStartsAProcessThatOutlivesTheInstaller(t *testing.T) {
	if _, err := startDetached(context.Background(), Command{Path: "true", Sudo: true}); err == nil {
		t.Fatal("a detached start with sudo was accepted")
	}
	pid := startSleeper(t)
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()
	if pid <= 0 || !processAlive(pid) {
		t.Fatalf("the detached process %d is not running", pid)
	}
}

func TestProbeHealth(t *testing.T) {
	for name, tc := range map[string]struct {
		code    int
		body    string
		version string
	}{
		"ok":         {http.StatusOK, `{"status":"ok","version":"fec47616","nodes":3,"sessions":3}`, "fec47616"},
		"no version": {http.StatusOK, "", ""},
		"error":      {http.StatusInternalServerError, `{"version":"fec47616"}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			version, err := probeHealth(context.Background(), srv.URL)
			if tc.code == http.StatusOK && err != nil {
				t.Fatalf("a healthy server reported %v", err)
			}
			if tc.code != http.StatusOK && err == nil {
				t.Fatal("an unhealthy server reported success")
			}
			if version != tc.version {
				t.Fatalf("version = %q, want %q", version, tc.version)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable := srv.URL
	srv.Close()
	if _, err := probeHealth(context.Background(), unreachable); err == nil {
		t.Fatal("an unreachable server reported success")
	}
	if _, err := probeHealth(context.Background(), "http://[::1]:not-a-port"); err == nil {
		t.Fatal("a malformed URL reported success")
	}
}

// A server of another version still answering on the port is not the started
// binary: the wait keeps polling until the expected version answers, and gives
// up naming the version it saw.
func TestWaitHealthyWaitsForTheExpectedVersion(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.deps.HealthTimeout = 5 * time.Second
	attempts := 0
	h.deps.Health = func(context.Context, string) (string, error) {
		attempts++
		if attempts < 3 {
			return "v1.0.0", nil
		}
		return "v2.0.0", nil
	}
	h.installer = New(h.layout, h.deps, h.io)
	if err := h.installer.waitHealthy(context.Background(), Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", "v2.0.0"); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}

	h.deps.HealthTimeout = 2 * time.Second
	h.deps.Health = func(context.Context, string) (string, error) { return "v1.0.0", nil }
	h.installer = New(h.layout, h.deps, h.io)
	err := h.installer.waitHealthy(context.Background(), Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", "v2.0.0")
	if err == nil || !strings.Contains(err.Error(), "reports version v1.0.0, not v2.0.0") {
		t.Fatalf("err = %v, want the stale version named", err)
	}
	// Without an expected version any healthy answer will do.
	if err := h.installer.waitHealthy(context.Background(), Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", ""); err != nil {
		t.Fatalf("waitHealthy without a version: %v", err)
	}
}

func TestWaitHealthyRetriesUntilTheServerAnswers(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.deps.HealthTimeout = 5 * time.Second
	attempts := 0
	h.deps.Health = func(context.Context, string) (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("connection refused")
		}
		return "", nil
	}
	h.installer = New(h.layout, h.deps, h.io)

	if err := h.installer.waitHealthy(context.Background(), Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", ""); err != nil {
		t.Fatalf("waitHealthy: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestWaitHealthyGivesUpWithTheLastError(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.deps.HealthTimeout = 3 * time.Second
	h.deps.Health = func(context.Context, string) (string, error) { return "", errors.New("connection refused") }
	h.installer = New(h.layout, h.deps, h.io)

	err := h.installer.waitHealthy(context.Background(), Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", "")
	if err == nil {
		t.Fatal("an unhealthy server was accepted")
	}
	if !strings.Contains(err.Error(), "connection refused") || !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("err = %v", err)
	}

	// A cancelled context stops the wait at once rather than spending the rest
	// of the timeout on a server nobody is waiting for any more.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.installer.waitHealthy(ctx, Service{Mode: ModeSystemd}, "http://127.0.0.1:8800/healthz", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestDepsDefaults(t *testing.T) {
	var d Deps
	if d.now().IsZero() {
		t.Fatal("the default clock is unset")
	}
	if d.client() == nil {
		t.Fatal("the default HTTP client is nil")
	}
	if d.healthTimeout() <= 0 || d.stopTimeout() <= 0 {
		t.Fatal("the default timeouts are unset")
	}
	if d.hasBinary("definitely-not-a-real-command-4f2a") {
		t.Fatal("a missing binary was reported as present")
	}
	if !d.hasBinary("sh") {
		t.Fatal("sh was not found on PATH")
	}
	if got := d.systemdRunning(); got != systemdAvailable() {
		t.Fatalf("systemdRunning = %v", got)
	}
	// The default sleep must return the moment the context is cancelled rather
	// than serving out a timeout nobody is waiting for.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	d.sleep(ctx, time.Minute)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the default sleep waited %s for a cancelled context", elapsed)
	}
	d.sleep(ctx, 0)
}

// startSleeper starts a real, detached process (its own session, released so
// this process never waits on it) that a test can signal. Nothing here reaps it
// when it exits, so processAlive has to recognise the zombie the kernel leaves
// behind, which is the same state the installer sees for the server it started.
func startSleeper(t *testing.T) int {
	t.Helper()
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep is not on PATH: %v", err)
	}
	pid, err := startDetached(context.Background(), Command{Path: path, Args: []string{"30"}})
	if err != nil {
		t.Fatalf("startSleeper: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	waitForArgv(t, pid)
	return pid
}

// waitForArgv blocks until /proc shows pid's argv. startDetached returns once
// the exec has succeeded, which Go learns from its close-on-exec pipe closing,
// and the kernel closes that pipe before it publishes the new argv: for a
// moment the process is named after the new binary (comm) yet /proc/<pid>/cmdline
// reads empty, which the identity check rightly treats as "argv unknown, not
// ours". A test that asserts on the identity of a process it just started has
// to wait that window out, or it passes or fails with the machine's load. The
// wait is bounded so a process that never gets an argv still fails the test.
func waitForArgv(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := readProcCmdline(pid)
		if err != nil || len(raw) > 0 {
			// Without /proc (macOS) the identity comes from ps, which has no
			// such window.
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never showed an argv in /proc", pid)
		}
		time.Sleep(time.Millisecond)
	}
}

// startSleeperIgnoringTERM starts a process that only SIGKILL can stop.
func startSleeperIgnoringTERM(t *testing.T) int {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "stubborn")
	writeFile(t, script, "#!/bin/sh\ntrap '' TERM\nwhile true; do sleep 1; done\n", 0o755)
	pid, err := startDetached(context.Background(), Command{Path: script})
	if err != nil {
		t.Fatalf("startSleeperIgnoringTERM: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	// The shell must have installed its trap before the test signals it,
	// otherwise the first SIGTERM would end it and the escalation would never be
	// reached.
	time.Sleep(300 * time.Millisecond)
	if !processAlive(pid) {
		t.Fatalf("the stubborn child %d did not start", pid)
	}
	return pid
}

// waitGone polls until the pid names no live process.
func waitGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return !processAlive(pid)
}

// resolve returns the canonical path, so a test can compare what the OS reports
// for a directory with the path it asked for.
func resolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("eval symlinks %s: %v", path, err)
	}
	return resolved
}

// A nohup installation has no supervisor, so once the process the start left
// behind is gone there is nothing left to wait for: the wait must end at once
// instead of spending the rest of the budget. A systemd unit is Restart=always,
// so the same dead service there keeps its retries.
func TestWaitHealthyStopsWhenAnUnsupervisedServerIsGone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     string
		pid      int
		attempts int
		wantErr  string
	}{
		{name: "nohup, the process exited", mode: ModeNohup, pid: deadPID, attempts: 1, wantErr: "no longer running"},
		{name: "nohup, still alive but not answering yet", mode: ModeNohup, pid: aliveServer, attempts: 4, wantErr: "did not become ready"},
		{name: "systemd keeps its retries", mode: ModeSystemd, pid: deadPID, attempts: 4, wantErr: "did not become ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			h := newHarness(t, dir)
			h.deps.HealthTimeout = 4 * time.Second
			attempts := 0
			h.deps.Health = func(context.Context, string) (string, error) {
				attempts++
				return "", errors.New("connection refused")
			}
			h.installer = New(h.layout, h.deps, h.io)
			pid := tc.pid
			if pid == aliveServer {
				pid = startStandIn(t, h.layout.Binary())
			}
			if err := writePID(h.layout.PIDFile(), pid); err != nil {
				t.Fatal(err)
			}
			svc := Service{Layout: h.layout, Mode: tc.mode, Deps: h.deps, IO: h.io}

			err := h.installer.waitHealthy(context.Background(), svc, "http://127.0.0.1:8800/healthz", "")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
			}
			if attempts != tc.attempts {
				t.Fatalf("health attempts = %d, want %d", attempts, tc.attempts)
			}
			// Whatever the reason, the operator is pointed at the log.
			if tc.mode == ModeNohup && tc.pid == deadPID && !strings.Contains(err.Error(), h.layout.LogFile()) {
				t.Fatalf("err = %v, want the log path", err)
			}
		})
	}
}

// deadPID is a pid no process can hold: above the kernel's default pid_max and
// therefore never recycled onto a live process during the test.
const deadPID = 0x7FFFFFF0

// An operator in /root/autodl-tmp runs `gpuworks upgrade --dir ./gpuworks/server`.
// The nohup service starts the binary from the install directory, so a layout
// kept relative resolved the binary a second time below it and the start failed
// with "not a directory" - for the new version and for the rollback alike.
func TestNohupStartFromARelativeInstallDirRunsTheInstalledBinary(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	layout := NewLayout("./gpuworks/server", "", "")
	if want := filepath.Join(root, "gpuworks", "server"); layout.Dir != want {
		t.Fatalf("dir = %q, want %q", layout.Dir, want)
	}
	if !filepath.IsAbs(layout.DataDir) || !filepath.IsAbs(layout.LogsDir) || !filepath.IsAbs(layout.BackupsDir) {
		t.Fatalf("layout keeps relative directories: %+v", layout)
	}
	if err := os.MkdirAll(layout.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "started")
	script := "#!/bin/sh\npwd > " + marker + ".tmp && mv " + marker + ".tmp " + marker + "\n"
	if err := os.WriteFile(layout.Binary(), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := Service{Layout: layout, Mode: ModeNohup, Cfg: UnitConfig{ConfigPath: layout.ConfigPath()}, Deps: Deps{StartDetached: startDetached}}
	if err := svc.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		dir, err := os.ReadFile(marker)
		if err == nil {
			if got, _ := filepath.EvalSymlinks(strings.TrimSpace(string(dir))); got != mustEvalSymlinks(t, layout.Dir) {
				t.Fatalf("the server ran in %q, want the install directory %q", got, layout.Dir)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the installed binary never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if readPID(layout.PIDFile()) == 0 {
		t.Fatal("no pid file was written in the install directory")
	}
}

func mustEvalSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// aliveServer asks for a running process that passes for the installed
// server: the identity check reads argv[0], so a sleep started under the
// binary's path stands in for it.
const aliveServer = -2

func startStandIn(t *testing.T, binary string) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep to stand in for the server")
	}
	cmd := &exec.Cmd{Path: sleep, Args: []string{binary, "30"}}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}
