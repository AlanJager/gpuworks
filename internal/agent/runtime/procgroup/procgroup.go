// Package procgroup tracks workloads that run as plain background process
// groups rather than inside a daemon-managed container: the udocker driver's
// PRoot launches and the native driver's macOS engine processes. A workload is
// a process group whose leader's pid is kept in <dir>/<name>.pid and whose exit
// status, once this process has reaped it, in <dir>/<name>.exit, so a restarted
// agent still finds the workloads an earlier one started.
package procgroup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// UnknownExitCode is reported for a workload whose end this process did not
// observe. Only the parent can read a child's exit status, so a workload
// started by an earlier agent process (and reaped by init after the agent
// restarted) has none on record. It is non-zero because a serving engine that
// stopped is a failure whatever its status was.
const UnknownExitCode = 1

// reapWaitLimit bounds how long a status read waits for the reaper to record a
// status it is about to write: the gap is only the time between wait(2)
// returning and the file being written.
const reapWaitLimit = 2 * time.Second

// Tracker keeps the pid and exit files of the workloads under Dir.
type Tracker struct {
	Dir string

	mu      sync.Mutex
	reaping map[string]chan struct{} // workloads whose exit status is not yet recorded
}

// PIDPath is where the group leader's pid of name is kept.
func (t *Tracker) PIDPath(name string) string { return filepath.Join(t.Dir, name+".pid") }

// ExitPath is where the exit status of name is recorded once reaped.
func (t *Tracker) ExitPath(name string) string { return filepath.Join(t.Dir, name+".exit") }

// Track records a just-started workload and starts reaping it. cmd must have
// been started as the leader of its own process group (Setpgid), because Stop
// signals the whole group. The exit status is written before the reap is
// marked done, so Status reports the code docker would (128+signal for a
// signal death, which is what lets the engine recognise an OOM kill from exit
// code 137). release runs after the reap, for the caller's log file.
func (t *Tracker) Track(name string, cmd *exec.Cmd, release func()) error {
	_ = os.Remove(t.ExitPath(name))
	err := os.WriteFile(t.PIDPath(name), []byte(strconv.Itoa(cmd.Process.Pid)), 0o644)
	done := make(chan struct{})
	t.mu.Lock()
	if t.reaping == nil {
		t.reaping = map[string]chan struct{}{}
	}
	t.reaping[name] = done
	t.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		_ = os.WriteFile(t.ExitPath(name), []byte(strconv.Itoa(ExitCodeOf(cmd.ProcessState))), 0o644)
		t.mu.Lock()
		if t.reaping[name] == done {
			delete(t.reaping, name)
		}
		t.mu.Unlock()
		close(done)
		if release != nil {
			release()
		}
	}()
	if err != nil {
		return fmt.Errorf("record pid of %s: %w", name, err)
	}
	return nil
}

// ExitCodeOf is a reaped process's status the way docker reports it.
func ExitCodeOf(state *os.ProcessState) int {
	if state == nil {
		return UnknownExitCode
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	if code := state.ExitCode(); code >= 0 {
		return code
	}
	return UnknownExitCode
}

// PID returns the recorded group leader of name.
func (t *Tracker) PID(name string) (int, bool) {
	b, err := os.ReadFile(t.PIDPath(name))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// Alive reports whether a process exists. Signal 0 probes existence without
// killing; EPERM means the process exists but belongs to another user (a
// login user inspecting the runtime user's workloads sees them that way).
func Alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// groupAlive reports whether any process of the group led by pid remains.
func groupAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || err == syscall.EPERM
}

// Status reports whether name is tracked, whether its leader is alive and,
// when it is not, the exit status recorded for it.
func (t *Tracker) Status(ctx context.Context, name string) (exists, running bool, exitCode int) {
	pid, ok := t.PID(name)
	if !ok {
		return false, false, 0
	}
	if Alive(pid) {
		return true, true, 0
	}
	return true, false, t.recordedExitCode(ctx, name)
}

// Running lists the tracked workloads whose leader is alive.
func (t *Tracker) Running() ([]string, error) {
	entries, err := os.ReadDir(t.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing has run on this node yet.
			return nil, nil
		}
		return nil, fmt.Errorf("list tracked workloads: %w", err)
	}
	var names []string
	for _, entry := range entries {
		name, tracked := strings.CutSuffix(entry.Name(), ".pid")
		if !tracked {
			continue
		}
		if pid, ok := t.PID(name); ok && Alive(pid) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Stop sends SIGTERM to the group of name and SIGKILL once grace has passed.
// It waits on the whole group, not its leader: an engine's workers can outlive
// a leader that exits on SIGTERM, and they hold the memory and port the next
// start needs. A workload that is not tracked or already gone is stopped.
func (t *Tracker) Stop(ctx context.Context, name string, grace time.Duration) error {
	pid, ok := t.PID(name)
	if !ok {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop process group of %s: %w", name, err)
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for groupAlive(pid) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("kill process group of %s: %w", name, err)
			}
			return nil
		case <-tick.C:
		}
	}
	return nil
}

// Forget drops the pid and exit files of a stopped workload, after letting a
// pending reaper record its status so the file is not left behind for the
// next workload of the same name.
func (t *Tracker) Forget(name string) {
	_ = os.Remove(t.PIDPath(name))
	t.awaitReap(context.Background(), name)
	_ = os.Remove(t.ExitPath(name))
}

// awaitReap waits, briefly, for a pending reaper of name to record its status.
func (t *Tracker) awaitReap(ctx context.Context, name string) {
	t.mu.Lock()
	done := t.reaping[name]
	t.mu.Unlock()
	if done == nil {
		return
	}
	timer := time.NewTimer(reapWaitLimit)
	defer timer.Stop()
	select {
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
}

// recordedExitCode returns the exit status of a workload whose process is gone.
func (t *Tracker) recordedExitCode(ctx context.Context, name string) int {
	t.awaitReap(ctx, name)
	data, err := os.ReadFile(t.ExitPath(name))
	if err != nil {
		return UnknownExitCode
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return UnknownExitCode
	}
	return code
}
