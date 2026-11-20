//go:build linux

package udocker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRunveilRemoveRetriesAStopWithAnUnreapedWorker(t *testing.T) {
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
	ready := filepath.Join(t.TempDir(), "ready")
	leader := exec.Command("/bin/sh", "-c", `trap '' TERM; : > "$1"; exec sleep 30`, "leader", ready)
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := leader.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-leader.Process.Pid, syscall.SIGKILL) })
	if err := d.procs().Track(id, leader, nil); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leader did not install the signal trap")
		}
	}
	worker := exec.Command("/bin/sh", "-c", "exit 0")
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Wait() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := d.Remove(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove with a cancelled stop = %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("a pending removal deleted the container: %v", err)
	}
	if _, ok := d.procs().PID(id); !ok {
		t.Fatal("a pending removal forgot the workload's process group")
	}

	if err := d.Remove(context.Background(), id); err != nil {
		t.Fatalf("Remove with only an unreaped worker left: %v", err)
	}
	if err := syscall.Kill(worker.Process.Pid, 0); err != nil {
		t.Fatalf("fixture's zombie was reaped before the test requested it: %v", err)
	}
	if _, err := os.Stat(d.containerDir(id)); !os.IsNotExist(err) {
		t.Fatalf("the completed removal left the container: %v", err)
	}
	if _, ok := d.procs().PID(id); ok {
		t.Fatal("the completed removal kept the workload's pid file")
	}
}
