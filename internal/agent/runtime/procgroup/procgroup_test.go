package procgroup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func start(t *testing.T, tr *Tracker, name, script string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Track(name, cmd, nil); err != nil {
		t.Fatal(err)
	}
	// Every test leaves the directory to TempDir; let the reaper finish first.
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		tr.awaitReap(context.Background(), name)
	})
	return cmd
}

func waitExited(t *testing.T, tr *Tracker, name string) (bool, int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		exists, running, code := tr.Status(context.Background(), name)
		if !running {
			return exists, code
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not exit", name)
	return false, 0
}

func TestStatusReportsTheExitStatusDockerWould(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	for _, tc := range []struct {
		name, script string
		want         int
	}{
		{"clean", "exit 0", 0},
		{"error", "exit 7", 7},
		// An OOM kill is SIGKILL, which docker reports as 137.
		{"killed", "kill -9 $$", 137},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start(t, tr, tc.name, tc.script)
			if exists, code := waitExited(t, tr, tc.name); !exists || code != tc.want {
				t.Fatalf("exists=%v code=%d, want exited with %d", exists, code, tc.want)
			}
		})
	}
}

func TestStopTerminatesTheWholeGroup(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	// The leader starts a child that must go down with it.
	childFile := filepath.Join(t.TempDir(), "child")
	start(t, tr, "serving", "sleep 30 & echo $! > "+childFile+"; wait")
	var child int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(childFile); err == nil && len(data) > 1 {
			_, _ = fmt.Sscan(string(data), &child)
			break
		}
	}
	if child == 0 {
		t.Fatal("child pid not recorded")
	}
	if err := tr.Stop(context.Background(), "serving", 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if exists, code := waitExited(t, tr, "serving"); !exists || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("exists=%v code=%d, want SIGTERM's 143", exists, code)
	}
	for deadline := time.Now().Add(5 * time.Second); Alive(child) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(child) {
		t.Fatalf("child %d survived the group stop", child)
	}
}

func TestStopEscalatesToSIGKILLAfterTheGrace(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	start(t, tr, "stubborn", "trap '' TERM; while :; do sleep 0.05; done")
	time.Sleep(100 * time.Millisecond) // let the shell install its trap
	began := time.Now()
	if err := tr.Stop(context.Background(), "stubborn", 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if exists, code := waitExited(t, tr, "stubborn"); !exists || code != 137 {
		t.Fatalf("exists=%v code=%d, want SIGKILL's 137", exists, code)
	}
	if elapsed := time.Since(began); elapsed < 200*time.Millisecond {
		t.Fatalf("killed after %s, before the grace ran out", elapsed)
	}
}

// A leader that exits on SIGTERM does not end the stop while a worker of its
// group ignores it: the worker still holds what the next start needs.
func TestStopWaitsForTheGroupNotJustItsLeader(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	childFile := filepath.Join(t.TempDir(), "child")
	start(t, tr, "serving", "(trap '' TERM; while :; do sleep 0.05; done) & echo $! > "+childFile+"; wait")
	var child int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(childFile); err == nil && len(data) > 1 {
			_, _ = fmt.Sscan(string(data), &child)
			break
		}
	}
	if child == 0 {
		t.Fatal("child pid not recorded")
	}
	time.Sleep(100 * time.Millisecond) // let the worker install its trap
	if err := tr.Stop(context.Background(), "serving", 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); Alive(child) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(child) {
		t.Fatalf("worker %d outlived the stop", child)
	}
}

func TestStopHonoursTheCallersContext(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	start(t, tr, "stubborn", "trap '' TERM; while :; do sleep 0.05; done")
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := tr.Stop(ctx, "stubborn", time.Minute); err != context.DeadlineExceeded {
		t.Fatalf("Stop = %v, want the context's deadline", err)
	}
}

func TestAWorkloadThisProcessDidNotReapHasAnUnknownNonZeroStatus(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	// Started by an earlier agent process: no reaper, no recorded status.
	if err := os.WriteFile(tr.PIDPath("adopted"), []byte("99999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if exists, running, code := tr.Status(context.Background(), "adopted"); !exists || running || code != UnknownExitCode {
		t.Fatalf("Status = %v %v %d", exists, running, code)
	}
	if exists, _, _ := tr.Status(context.Background(), "never-started"); exists {
		t.Fatal("an untracked name exists")
	}
	// Stopping what is not there succeeds.
	if err := tr.Stop(context.Background(), "never-started", time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestRunningListsWhatStatusReportsRunning(t *testing.T) {
	tr := &Tracker{Dir: filepath.Join(t.TempDir(), "state")}
	// No directory yet is an idle node, not a failure.
	if names, err := tr.Running(); err != nil || names != nil {
		t.Fatalf("Running on a fresh node = %v, %v", names, err)
	}
	if err := os.MkdirAll(tr.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	start(t, tr, "alive", "sleep 30")
	start(t, tr, "exited", "exit 3")
	waitExited(t, tr, "exited")
	for name, content := range map[string]string{"garbled.pid": "not-a-pid", "alive.log": "serving\n", "zero.pid": "0"} {
		if err := os.WriteFile(filepath.Join(tr.Dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	names, err := tr.Running()
	if err != nil || !slices.Equal(names, []string{"alive"}) {
		t.Fatalf("Running = %v, %v", names, err)
	}

	// An unreadable state directory is an error, not an empty node.
	broken := &Tracker{Dir: filepath.Join(t.TempDir(), "file")}
	if err := os.WriteFile(broken.Dir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if names, err := broken.Running(); err == nil || names != nil {
		t.Fatalf("Running on a file = %v, %v", names, err)
	}
}

func TestForgetLeavesNothingForTheNextWorkloadOfTheName(t *testing.T) {
	tr := &Tracker{Dir: t.TempDir()}
	start(t, tr, "gone", "exit 5")
	waitExited(t, tr, "gone")
	tr.Forget("gone")
	for _, path := range []string{tr.PIDPath("gone"), tr.ExitPath("gone")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived Forget: %v", path, err)
		}
	}
}

func TestTrackReportsAPidItCannotRecord(t *testing.T) {
	tr := &Tracker{Dir: filepath.Join(t.TempDir(), "missing")}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := tr.Track("x", cmd, nil); err == nil {
		t.Fatal("Track succeeded without a state directory")
	}
	tr.awaitReap(context.Background(), "x")
}
