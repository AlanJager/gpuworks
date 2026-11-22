//go:build linux

package procgroup

import (
	"log/slog"
	"os"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var adoptOnce sync.Once

// AdoptOrphans makes this process the parent the kernel hands an exited
// descendant to once that descendant's own parent is gone, instead of the
// init of its PID namespace. It is done once for the process and kept for the
// process's lifetime: the attribute belongs to the OS process rather than to
// a tracker, and a child does not inherit it, so only orphans from below this
// process arrive here. It is idempotent: a launcher calls it before it
// starts a workload, and Track again for whatever starts workloads without
// going through one.
//
// Without it a teardown cannot clean up after itself. Only a parent may
// wait(2) for a process, and an engine does not wait for every worker it
// started: the patched PRoot kills its tracees as it exits (--kill-on-exit)
// in no particular order, so a worker whose parent died first is reparented
// away from this process while it is being killed. On the rootless nodes the
// udocker driver exists for, the init it lands on is the container image's
// entrypoint, which does not reap, so the worker holds a process-table slot
// as <defunct> until the node reboots - one batch per deployment that ran.
func AdoptOrphans() {
	adoptOnce.Do(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			slog.Warn("this process cannot adopt the orphaned processes of its workloads; a worker whose parent exits first stays <defunct> under init", "err", err)
		}
	})
}

// reapOrphans waits for the exited members of the group led by pgid that this
// process has been handed, over the real process table.
func reapOrphans(pgid int) int { return reapGroupOrphans("/proc", pgid, os.Getpid()) }

// reapGroupOrphans waits for every exited process of the group led by pgid
// whose parent is now parent, except the group leader, and returns how many
// it reaped.
//
// The process group is the whole filter, and it has to be. A zombie child of
// this process may be a command whose exec.Cmd has not called Wait yet, and
// waiting for it here would take the exit status that caller is owed; nothing
// in the agent starts a command inside a workload's group, so a group member
// is either the launch itself or something the launch started. The leader is
// excluded because the exec.Cmd that started it needs its status to tell an
// out-of-memory kill from a clean exit (Track).
func reapGroupOrphans(procDir string, pgid, parent int) int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return 0
	}
	reaped := 0
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == pgid {
			continue
		}
		stat, err := readProcStat(procDir, entry.Name())
		if err != nil || stat.pgid != pgid || stat.ppid != parent || stat.state != 'Z' {
			continue
		}
		var status syscall.WaitStatus
		if waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err == nil && waited == pid {
			reaped++
		}
	}
	return reaped
}
