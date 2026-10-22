package selfinstall

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// inExec makes /proc/<pid>/cmdline read empty for pid, as Linux shows a
// process that has passed fork but whose exec has not finished loading the
// binary (seen on a loaded CI host: state D, cmdline "", ps "[gpuworks]").
func inExec(t *testing.T, pid int) {
	t.Helper()
	real := readProcCmdline
	readProcCmdline = func(p int) ([]byte, error) {
		if p == pid {
			return []byte{}, nil
		}
		return real(p)
	}
	t.Cleanup(func() { readProcCmdline = real })
}

func TestAProcessStillInExecIsNeitherOursNorGone(t *testing.T) {
	h := newHarness(t, t.TempDir())
	ctx := context.Background()
	pid := startSleeper(t)
	sleeper, _ := exec.LookPath("sleep")
	inExec(t, pid)
	// What ps prints for such a process; it must not be taken for argv.
	h.run.output["ps -p "+strconv.Itoa(pid)+" -o command="] = "[sleep]"

	if argv := cmdline(ctx, h.run, pid); argv != nil {
		t.Fatalf("cmdline of a process in exec = %q, want unknown", argv)
	}
	if h.run.ran("ps ") {
		t.Fatal("an empty /proc entry fell back to ps, which answers [comm]")
	}
	if isOurServer(ctx, h.run, pid, sleeper) {
		t.Fatal("a process whose argv is unknown was claimed as ours: stop would signal it")
	}
	if !mayBeOurServer(ctx, h.run, pid, sleeper) {
		t.Fatal("a server still loading its binary was reported gone")
	}
}

func TestPsBracketedCommandIsNotAnArgv(t *testing.T) {
	h := newHarness(t, t.TempDir())
	real := readProcCmdline
	readProcCmdline = func(int) ([]byte, error) { return nil, os.ErrNotExist } // no /proc, as on macOS
	t.Cleanup(func() { readProcCmdline = real })
	h.run.output["ps -p 4242 -o command="] = "[gpuworks]"
	if argv := cmdline(context.Background(), h.run, 4242); argv != nil {
		t.Fatalf("cmdline = %q, want unknown", argv)
	}
}

func TestNohupServiceStillInExecIsActive(t *testing.T) {
	h := newHarness(t, t.TempDir())
	pid := startSleeper(t)
	sleeper, _ := exec.LookPath("sleep")
	if err := writePID(h.layout.PIDFile(), pid); err != nil {
		t.Fatal(err)
	}
	svc := Service{Layout: h.layout, Mode: ModeNohup, Deps: h.deps}
	// The pid file names a process running another program: not active.
	if svc.Active(context.Background()) {
		t.Fatalf("pid %d running %s counted as the server %s", pid, sleeper, h.layout.Binary())
	}
	inExec(t, pid)
	if !svc.Active(context.Background()) {
		t.Fatal("the server just started, still in exec, counted as not running")
	}
}

func TestNohupStopPreservesAnUnidentifiedLiveProcess(t *testing.T) {
	h := newHarness(t, t.TempDir())
	pid := startSleeper(t)
	if err := writePID(h.layout.PIDFile(), pid); err != nil {
		t.Fatal(err)
	}
	inExec(t, pid)
	svc := Service{Layout: h.layout, Mode: ModeNohup, Deps: h.deps}
	if err := svc.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot identify") {
		t.Fatalf("stop unidentified process = %v", err)
	}
	if readPID(h.layout.PIDFile()) != pid || !processAlive(pid) {
		t.Fatal("stop lost the retry handle or signalled an unidentified process")
	}
}

// The failure this guards against: install started the server, the first
// health probe found the port closed while the server was still loading its
// binary, and the wait gave up at once with "the server is no longer running".
func TestWaitHealthyWaitsForAServerStillInExec(t *testing.T) {
	h := newHarness(t, t.TempDir())
	pid := startSleeper(t)
	if err := writePID(h.layout.PIDFile(), pid); err != nil {
		t.Fatal(err)
	}
	inExec(t, pid)
	refused := errors.New("dial tcp 127.0.0.1:8800: connect: connection refused")
	probes := 0
	deps := h.deps
	deps.Health = func(context.Context, string) (string, error) {
		probes++
		if probes < 3 {
			return "", refused
		}
		return "v1.2.3", nil
	}
	deps.HealthTimeout = 10 * time.Second
	installer := New(h.layout, deps, h.io)
	svc := Service{Layout: h.layout, Mode: ModeNohup, Deps: deps}
	if err := installer.waitHealthy(context.Background(), svc, "http://127.0.0.1:8800/healthz", "v1.2.3"); err != nil {
		t.Fatalf("waitHealthy = %v after %d probes", err, probes)
	}
	if probes != 3 {
		t.Fatalf("probes = %d, want 3", probes)
	}

	// A process that is known to run something else still ends the wait at once.
	real := readProcCmdline
	readProcCmdline = func(int) ([]byte, error) { return []byte("/usr/bin/other\x00"), nil }
	t.Cleanup(func() { readProcCmdline = real })
	probes = 0
	err := installer.waitHealthy(context.Background(), svc, "http://127.0.0.1:8800/healthz", "v1.2.3")
	if err == nil || !strings.Contains(err.Error(), "no longer running") || probes != 1 {
		t.Fatalf("waitHealthy = %v after %d probes", err, probes)
	}
}
