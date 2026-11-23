package procgroup

// A drain is the stop of a target that may hold accelerator resources: a
// SIGTERM, a SIGKILL once the graceful window has passed, and the exited
// descendants this process has adopted reaped as they appear. The Tracker
// stops its workloads through it (Stop), and callers with no pid file for a
// target use Drain or DrainProcess.

import (
	"context"
	"fmt"
	"syscall"
	"time"
)

// Drain stops the process group led by pgid the way Stop stops a tracked
// workload's, for a group this process has no pid file for: a launch whose
// record was never written or has already been forgotten.
func Drain(ctx context.Context, pgid int, grace, killWait time.Duration) error {
	return drainGroup(ctx, pgid, fmt.Sprintf("process group %d", pgid), grace, killWait, liveGroup)
}

// DrainProcess stops one process without signalling its process group, for a
// leftover that shares its group with processes this one must not signal. It
// reaps nothing: a member of a shared group cannot be told from a command
// this process is waiting for, and waiting for it would take the exit status
// that caller is owed.
func DrainProcess(ctx context.Context, pid int, grace, killWait time.Duration) error {
	return drain(ctx, drainTarget{
		signal:  pid,
		what:    fmt.Sprintf("process %d", pid),
		present: func() bool { return Alive(pid) },
		working: func() bool { return processStillWorking(pid) },
	}, grace, killWait)
}

// groupProbe is how a drain observes a process group. Tests substitute both
// halves, so a stop can be exercised on a group the test describes rather
// than one it has to produce.
type groupProbe struct {
	// stillWorking reports whether a group that answers signal 0 after
	// SIGKILL holds more than exited processes awaiting a wait(2).
	stillWorking func(pgid int) bool
	// reap waits for the group's exited members this process has been
	// handed, except its leader, and returns how many it reaped.
	reap func(pgid int) int
}

// liveGroup observes the real process table.
var liveGroup = groupProbe{stillWorking: groupStillWorking, reap: reapOrphans}

func drainGroup(ctx context.Context, pgid int, what string, grace, killWait time.Duration, probe groupProbe) error {
	return drain(ctx, drainTarget{
		signal:  -pgid,
		what:    what,
		present: func() bool { return groupAlive(pgid) },
		working: func() bool { return probe.stillWorking(pgid) },
		reap:    func() int { return probe.reap(pgid) },
	}, grace, killWait)
}

// drainTarget is what one drain signals and waits for.
type drainTarget struct {
	// signal is the kill(2) target: a pid, or -pgid for a whole group.
	signal int
	// what names the target in an error.
	what string
	// present reports whether anything of the target is still there.
	present func() bool
	// working reports whether, after SIGKILL, it still holds resources.
	working func() bool
	// reap collects what of the target this process may wait for; nil when
	// none of it may be.
	reap func() int
}

func drain(ctx context.Context, target drainTarget, grace, killWait time.Duration) error {
	if err := syscall.Kill(target.signal, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("stop %s: %w", target.what, err)
	}
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	killed := false
	for {
		// Reaping first is what lets the target go away at all: an exited
		// process keeps answering signal 0 until someone waits for it.
		if target.reap != nil {
			target.reap()
		}
		if !target.present() || (killed && !target.working()) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			if killed {
				return fmt.Errorf("%s is %w (kill wait %s)", target.what, ErrNotDrained, killWait)
			}
			if err := syscall.Kill(target.signal, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return fmt.Errorf("kill %s: %w", target.what, err)
			}
			killed = true
			deadline.Reset(killWait)
		case <-tick.C:
		}
	}
}
