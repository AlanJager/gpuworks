package udocker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
)

// startTracked starts script the way Run starts a workload: a pidfile in the
// state directory and a reaper that records the exit status.
func startTracked(t *testing.T, d *Driver, name, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // Stop signals the group
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := d.procs().Track(name, cmd, nil); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitExited(t *testing.T, d *Driver, name string) runtime.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := d.Status(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not exit", name)
	return runtime.Status{}
}

func TestStatusReportsTheWorkloadsOwnExitStatus(t *testing.T) {
	d := New(t.TempDir())
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, script string
		want         int
	}{
		{name: "clean", script: "exit 0", want: 0},
		{name: "error", script: "exit 7", want: 7},
		// A signal death reads as docker reports it, so an OOM kill (SIGKILL)
		// is exit code 137 here too.
		{name: "killed", script: "kill -9 $$", want: 137},
	} {
		t.Run(tc.name, func(t *testing.T) {
			startTracked(t, d, tc.name, tc.script)
			st := waitExited(t, d, tc.name)
			if !st.Exists || st.Running || st.ExitCode != tc.want {
				t.Fatalf("Status = %+v, want exited with %d", st, tc.want)
			}
		})
	}
}

func TestStatusKeepsTheRunningAnswerUntilTheWorkloadIsReaped(t *testing.T) {
	d := New(t.TempDir())
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := startTracked(t, d, "serving", "sleep 30")
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	st, err := d.Status(context.Background(), "serving")
	if err != nil || !st.Exists || !st.Running {
		t.Fatalf("running Status = %+v, err=%v", st, err)
	}
	if err := d.Stop(context.Background(), "serving"); err != nil {
		t.Fatal(err)
	}
	st = waitExited(t, d, "serving")
	if st.ExitCode != 128+15 {
		t.Fatalf("stopped Status = %+v, want SIGTERM's 143", st)
	}
	// A removed container leaves no status behind for its successor.
	d.forgetState("serving")
	if _, err := os.Stat(d.procs().ExitPath("serving")); !os.IsNotExist(err) {
		t.Fatalf("exit status survived removal: %v", err)
	}
}

func TestStatusOfAWorkloadThisDriverDidNotReapIsUnknownNonZero(t *testing.T) {
	d := New(t.TempDir())
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Started by an earlier agent process: no reaper, no recorded status.
	if err := os.WriteFile(filepath.Join(d.stateDir, "adopted.pid"), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := d.Status(context.Background(), "adopted")
	if err != nil || !st.Exists || st.Running || st.ExitCode != procgroup.UnknownExitCode {
		t.Fatalf("Status = %+v, err=%v", st, err)
	}
}

// The listing and Status read the same pid files, so they agree: a live
// workload is listed, one that has exited is not, and neither is a pid file
// that cannot be read as a pid.
func TestRunningContainersListsTheWorkloadsStatusReportsRunning(t *testing.T) {
	d := New(t.TempDir())
	// A node that has never run a workload has no state directory; that is an
	// empty listing, not a failure.
	if names, err := d.RunningContainers(context.Background()); err != nil || len(names) != 0 {
		t.Fatalf("unprovisioned node = %v, err=%v", names, err)
	}
	if err := os.MkdirAll(d.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if names, err := d.RunningContainers(context.Background()); err != nil || len(names) != 0 {
		t.Fatalf("idle node = %v, err=%v", names, err)
	}

	alive := startTracked(t, d, "gpuworks-alive", "sleep 30")
	// Killing the workload is not the end of it: the reaper wakes from Wait
	// and records the exit status, so the cleanup has to let it finish before
	// TempDir removes the directory it writes into. Without the wait the
	// removal and the write race, and the test fails in cleanup with
	// "directory not empty".
	t.Cleanup(func() {
		_ = syscall.Kill(-alive.Process.Pid, syscall.SIGKILL)
		d.procs().Forget("gpuworks-alive")
	})
	startTracked(t, d, "gpuworks-exited", "exit 3")
	waitExited(t, d, "gpuworks-exited")
	for name, content := range map[string]string{"gpuworks-garbled.pid": "not-a-pid", "gpuworks-alive.log": "serving\n"} {
		if err := os.WriteFile(filepath.Join(d.stateDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	names, err := d.RunningContainers(context.Background())
	if err != nil {
		t.Fatalf("RunningContainers: %v", err)
	}
	if want := []string{"gpuworks-alive"}; !slices.Equal(names, want) {
		t.Fatalf("RunningContainers = %v, want %v", names, want)
	}
	for _, name := range []string{"gpuworks-alive", "gpuworks-exited", "gpuworks-garbled"} {
		st, err := d.Status(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if listed := slices.Contains(names, name); listed != (st.Exists && st.Running) {
			t.Fatalf("%s: listed=%v but Status=%+v", name, listed, st)
		}
	}
}

func TestRunningContainersReportsAnUnreadableStateDirectory(t *testing.T) {
	d := New(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(d.stateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.stateDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	names, err := d.RunningContainers(context.Background())
	if err == nil || names != nil {
		t.Fatalf("RunningContainers = %v, err=%v, want an error and no names", names, err)
	}
}
