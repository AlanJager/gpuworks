//go:build linux

package udocker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
	"gpuworks.ai/gpuworks/internal/proto"
)

// preparedContainer is a container directory with the root filesystem a
// launch runs against, plus the state directory the tracker writes in.
func preparedContainer(t *testing.T, d *Driver, id string) string {
	t.Helper()
	root := filepath.Join(d.containerDir(id), "ROOT")
	for _, dir := range []string{d.stateDir, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// startLaunch starts a process that looks like a launch of this container:
// the root filesystem in its argv, where PRoot takes it as -r, and a process
// group of its own, as every launch this driver starts has. The script
// touches $READY once it is set up, and this returns only then: a stop that
// arrives while the shell is still installing its signal disposition would
// otherwise be a different test than the one written. Whatever the script
// does only after $GO exists happens after the returned go-ahead is given.
func startLaunch(t *testing.T, root, script string) (*exec.Cmd, func()) {
	t.Helper()
	dir := t.TempDir()
	ready, goAhead := filepath.Join(dir, "ready"), filepath.Join(dir, "go")
	cmd := exec.Command("/bin/sh", "-c", script, "proot-stand-in", root)
	cmd.Env = append(os.Environ(), "READY="+ready, "GO="+goAhead)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	release := func() {
		if err := os.WriteFile(goAhead, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			return cmd, release
		}
		if time.Now().After(deadline) {
			t.Fatal("the launch never reported itself set up")
		}
	}
}

func awaitPID(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(path)
		if err != nil || len(raw) == 0 {
			continue
		}
		if pid, err := strconv.Atoi(string(raw)); err == nil && pid > 0 {
			return pid
		}
	}
	t.Fatalf("%s never named a pid", path)
	return 0
}

func assertProcessGone(t *testing.T, what string, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		raw, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		t.Fatalf("%s (%d) is still in the process table: kill(0) = %v, stat %q", what, pid, err, raw)
	}
}

// Run starts the launch and writes its pid file afterwards. A teardown that
// arrives in between finds no record of the workload, so the stop signals
// nothing - and then the rootfs the launch is running against is deleted
// under it. The sweep recognises it by that rootfs.
func TestRemoveStopsALaunchStartedButNotYetRecorded(t *testing.T) {
	d := New(t.TempDir())
	d.runveil = true
	const id = "gpuworks-starting"
	root := preparedContainer(t, d, id)
	launch, _ := startLaunch(t, root, `: > "$READY"; while :; do sleep 1; done`)
	// Run's own reaper, which it attaches as soon as it has started the
	// launch (Track); the pid file is what the teardown is missing.
	reaped := make(chan struct{})
	go func() { _, _ = launch.Process.Wait(); close(reaped) }()
	if _, recorded := d.procs().PID(id); recorded {
		t.Fatal("the fixture recorded a pid file; the race under test is the one without")
	}

	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove while the launch was still starting: %v", err)
	}
	select {
	case <-reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("the launch was never stopped")
	}
	assertProcessGone(t, "the launch", launch.Process.Pid)
	if _, err := os.Stat(d.containerDir(id)); !os.IsNotExist(err) {
		t.Fatalf("the container is still there: %v", err)
	}
}

// The same leftover appears with the record already gone: an earlier teardown
// forgot it (forgetState), or an earlier agent process died holding it. What
// the launch started must go with it, reaped and not merely killed.
func TestRemoveStopsAndReapsALaunchWhoseRecordIsGone(t *testing.T) {
	d := New(t.TempDir())
	d.runveil = true
	const id = "gpuworks-forgotten"
	root := preparedContainer(t, d, id)
	workerFile := filepath.Join(t.TempDir(), "worker")
	// The worker's own parent exits at once, which hands the worker to this
	// process: only a wait(2) from here can clear it afterwards. It is made
	// after the launch has been recorded, as a running engine makes workers
	// long after the launch that started it.
	launch, release := startLaunch(t, root, fmt.Sprintf(`: > "$READY"; while [ ! -e "$GO" ]; do sleep 0.02; done; /bin/sh -c 'sleep 300 & printf %%s "$!" > %s' ; while :; do sleep 1; done`, workerFile))
	if err := d.procs().Track(id, launch, nil); err != nil {
		t.Fatal(err)
	}
	release()
	worker := awaitPID(t, workerFile)
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if parent := parentOf(t, worker); parent == os.Getpid() {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("the worker was never handed to this process: ppid %d", parent)
		}
	}
	if err := os.Remove(d.procs().PIDPath(id)); err != nil {
		t.Fatal(err)
	}

	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove with the launch's record gone: %v", err)
	}
	assertProcessGone(t, "the launch", launch.Process.Pid)
	assertProcessGone(t, "the launch's worker", worker)
}

// parentOf is the pid that may wait(2) for this process.
func parentOf(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		t.Fatalf("read the status of %d: %v", pid, err)
	}
	// comm is parenthesized and can itself contain spaces and parentheses;
	// the tail starts with state, then PPID.
	fields := bytes.Fields(raw[bytes.LastIndexByte(raw, ')')+1:])
	if len(fields) < 2 {
		t.Fatalf("status of %d is unreadable: %q", pid, raw)
	}
	parent, err := strconv.Atoi(string(fields[1]))
	if err != nil {
		t.Fatalf("parse the parent of %d (%q): %v", pid, raw, err)
	}
	return parent
}

// Only this container's own launches may be stopped. A path that merely
// starts with the root filesystem's is another container's or a renamed
// leftover, and this process is never its own leftover.
func TestLingeringLaunchPIDsMatchOnlyTheContainersOwnRoot(t *testing.T) {
	proc := t.TempDir()
	root := "/opt/gpuworks/udocker/containers/gpuworks-wanted/ROOT"
	write := func(pid int, argv ...string) {
		t.Helper()
		dir := filepath.Join(proc, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		line := ""
		for _, arg := range argv {
			line += arg + "\x00"
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(11, "/opt/gpuworks/udocker/bin/proot-arm64", "--kill-on-exit", "-r", root, "-w", "/", "vllm", "serve")
	write(12, "/opt/gpuworks/udocker/bin/proot-arm64", "-r", root+".old", "sleep")
	write(13, "/opt/gpuworks/udocker/bin/proot-arm64", "-r", "/opt/gpuworks/udocker/containers/gpuworks-other/ROOT")
	write(14, "/usr/bin/grep", "-r", root, "/var/log")
	write(15)
	write(16, "/opt/gpuworks/bin/gpuworks-agent", "run", "--udocker-dir", filepath.Dir(filepath.Dir(root)))
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := lingeringLaunchPIDs(proc, []string{root}, 14)
	if len(got) != 1 || got[0] != 11 {
		t.Fatalf("lingering launches = %v, want only the launch of this container (11)", got)
	}
}

// A teardown that could not finish says so with a code, not a sentence: the
// stop that left processes running has its own, and anything else is a
// removal still pending. The cause stays wrapped for the log and for
// errors.Is.
func TestTeardownFailureStatesTheOutcomeAsACode(t *testing.T) {
	cause := fmt.Errorf("stop process group of gpuworks-x: %w (kill wait 15s)", procgroup.ErrNotDrained)
	err := teardownFailure("gpuworks-x", cause)
	if code := runtime.TeardownFailureCode(err); code != proto.ErrorContainerProcessesNotDrained {
		t.Fatalf("code of a stop that left processes running = %q", code)
	}
	if !errors.Is(err, procgroup.ErrNotDrained) {
		t.Fatal("the coded failure dropped its cause")
	}
	var teardown *runtime.TeardownError
	if !errors.As(err, &teardown) || teardown.Container != "gpuworks-x" || teardown.Error() != cause.Error() {
		t.Fatalf("teardown error = %#v, want the container and the cause's own text", teardown)
	}

	other := teardownFailure("gpuworks-x", errors.New("remove container rootfs: permission denied"))
	if code := runtime.TeardownFailureCode(other); code != proto.ErrorContainerRemovalPending {
		t.Fatalf("code of any other teardown failure = %q", code)
	}
}

// A cancelled teardown keeps the container and reports the pending removal as
// a code, with the cancellation still recognisable.
func TestRemoveReportsACancelledTeardownAsAPendingRemoval(t *testing.T) {
	d := New(t.TempDir())
	d.runveil = true
	const id = "gpuworks-cancelled"
	root := preparedContainer(t, d, id)
	// After startLaunch kills the group, wait for Track to write its exit
	// status before TempDir removes the state directory.
	var reaper sync.WaitGroup
	t.Cleanup(reaper.Wait)
	launch, _ := startLaunch(t, root, `trap '' TERM; : > "$READY"; while :; do sleep 1; done`)
	reaper.Add(1)
	if err := d.procs().Track(id, launch, reaper.Done); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := d.Remove(ctx, id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove with a cancelled stop = %v, want the cancellation", err)
	}
	if code := runtime.TeardownFailureCode(err); code != proto.ErrorContainerRemovalPending {
		t.Fatalf("code of a cancelled teardown = %q", code)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("a pending removal deleted the container: %v", err)
	}
}
