package udocker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime/procgroup"
)

// A launch this driver does not track has no graceful shutdown left to
// protect - nobody is reading its output or its exit status - so it gets a
// shorter window than a tracked workload before the kill, and the same wait
// afterwards for an engine that releases accelerators as it goes down.
const (
	lingeringLaunchGrace    = 2 * time.Second
	lingeringLaunchKillWait = 10 * time.Second
)

// hostProcDir is the process table the sweep below reads.
const hostProcDir = "/proc"

// stopLingeringLaunches stops the launches of a container that the tracker
// has no pid file for, so removing the rootfs cannot leave a PRoot running
// against a directory that is no longer there.
//
// A tracked workload goes down through its pid file (Stop). A launch without
// one is left behind: Run writes the pid file after the process has started,
// so a teardown that arrives in between never signals it; the record is also
// gone once a teardown forgot it (forgetState) or an earlier agent process
// died before writing it; and a terminal or exec session starts a PRoot of
// its own that was never tracked at all.
//
// A launch is recognised by the container root filesystem in its argv, which
// runveil.Prepare passes as PRoot's -r and the udocker CLI passes the same
// way: a path under this driver's UDOCKER_DIR that no other container can
// produce. Each one is stopped through its own process group, because a PRoot
// that is killed outright never kills the tracees it would have taken with it
// (--kill-on-exit).
func (d *Driver) stopLingeringLaunches(ctx context.Context, id string) error {
	pids := lingeringLaunchPIDs(hostProcDir, d.containerRootPaths(id), os.Getpid())
	if len(pids) == 0 {
		return nil
	}
	ownGroup, ownGroupErr := syscall.Getpgid(0)
	var failures []error
	for _, pid := range pids {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		group, err := syscall.Getpgid(pid)
		switch {
		case errors.Is(err, syscall.ESRCH):
			// It exited on its own while the process table was being read.
			continue
		case err != nil:
			failures = append(failures, fmt.Errorf("read the process group of launch %d: %w", pid, err))
			continue
		case group > 1 && ownGroupErr == nil && group != ownGroup:
			slog.Warn("stopping a container launch this driver does not track", "container", id, "pid", pid, "pgid", group)
			err = procgroup.Drain(ctx, group, lingeringLaunchGrace, lingeringLaunchKillWait)
		default:
			// It shares its process group with this agent: signalling the
			// group would signal the agent itself.
			slog.Warn("stopping a container launch this driver does not track, without its process group", "container", id, "pid", pid, "pgid", group)
			err = procgroup.DrainProcess(ctx, pid, lingeringLaunchGrace, lingeringLaunchKillWait)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("stop launch %d of container %s: %w", pid, id, err))
		}
	}
	return errors.Join(failures...)
}

// containerRootPaths are the spellings of a container's root filesystem that
// a launch can carry: the managed path, and what it resolves to when the
// container name is an alias `udocker create --name=` made for it.
func (d *Driver) containerRootPaths(id string) []string {
	root := filepath.Join(d.containerDir(id), "ROOT")
	paths := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		paths = append(paths, resolved)
	}
	return paths
}

// lingeringLaunchPIDs are the processes whose argv names one of roots, this
// process excluded. The match is on a whole argument: a path that merely
// starts with a container's root - another container under the same
// directory, or a renamed leftover of this one - belongs to something else.
func lingeringLaunchPIDs(procDir string, roots []string, self int) []int {
	entries, err := os.ReadDir(procDir)
	if err != nil {
		return nil
	}
	var pids []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		argv, err := os.ReadFile(filepath.Join(procDir, entry.Name(), "cmdline"))
		if err != nil || len(argv) == 0 {
			continue
		}
		if argvNamesPath(argv, roots) {
			pids = append(pids, pid)
		}
	}
	sort.Ints(pids)
	return pids
}

// argvNamesPath reports whether the NUL-separated argv holds one of paths as
// a whole argument.
func argvNamesPath(argv []byte, paths []string) bool {
	for _, arg := range strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00") {
		for _, path := range paths {
			if arg == path {
				return true
			}
		}
	}
	return false
}
