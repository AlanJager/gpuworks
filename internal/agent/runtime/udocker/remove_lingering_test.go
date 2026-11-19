package udocker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Remove returning nil is what the engine reports as the instance torn down,
// after which its cards are no longer the platform's. A process of the
// workload that is still there after SIGKILL still holds its card: the
// removal must fail and keep everything a retry needs, and succeed once the
// group is gone. The worker is a child of this test in the workload's group
// that nobody reaps - present after the kill for as long as the test says.
func TestRunveilRemoveStaysPendingWhileAKilledProcessIsStillThere(t *testing.T) {
	d := New(t.TempDir())
	d.runveil = true
	d.tracker.KillWait = 200 * time.Millisecond
	const id = "gpuworks-follower"
	root := filepath.Join(d.containerDir(id), "ROOT")
	for _, dir := range []string{d.stateDir, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const stubborn = "trap '' TERM; while :; do sleep 0.05; done"
	leader := exec.Command("/bin/sh", "-c", stubborn)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	if err := d.procs().Track(id, leader, nil); err != nil {
		t.Fatal(err)
	}
	worker := exec.Command("/bin/sh", "-c", stubborn)
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		_ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL)
		if !reaped {
			_ = worker.Wait()
		}
	})
	time.Sleep(100 * time.Millisecond) // let the shells install their traps

	err := d.Remove(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "still alive") {
		t.Fatalf("Remove with a killed process still in the group = %v, want it to fail", err)
	}
	if _, statErr := os.Stat(root); statErr != nil {
		t.Fatalf("a pending removal deleted the container: %v", statErr)
	}
	if _, ok := d.procs().PID(id); !ok {
		t.Fatal("a pending removal forgot the workload's process group; a retry could not find it")
	}

	_ = worker.Wait()
	reaped = true
	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove once the group is gone = %v", err)
	}
	if _, statErr := os.Stat(d.containerDir(id)); !os.IsNotExist(statErr) {
		t.Fatalf("the completed removal left the container: %v", statErr)
	}
	if _, ok := d.procs().PID(id); ok {
		t.Fatal("the completed removal kept the workload's pid file")
	}
}
