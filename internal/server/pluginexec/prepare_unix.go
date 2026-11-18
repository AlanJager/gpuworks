//go:build linux || darwin

package pluginexec

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// checkDescriptors makes sure the plugin gets what gwx/1 promises: its two
// sockets on ServeFD and HostFD, and /dev/null on stdin and stdout, which
// carry nothing (logs go to stderr).
func checkDescriptors() error {
	for _, fd := range []int{ServeFD, HostFD} {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			return fmt.Errorf("descriptor %d: %w", fd, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
			return fmt.Errorf("descriptor %d is not a socket", fd)
		}
		// The plugin must inherit it.
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, 0); err != nil {
			return fmt.Errorf("descriptor %d: %w", fd, err)
		}
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	for _, fd := range []int{0, 1} {
		if err := unix.Dup2(int(null.Fd()), fd); err != nil {
			return fmt.Errorf("descriptor %d to %s: %w", fd, os.DevNull, err)
		}
	}
	return nil
}

// setLimits applies the resource limits and nice value. setpriority with
// who=0 changes the calling thread on Linux and the process on macOS; the
// trampoline runs on the thread that will execute the plugin either way.
func setLimits(p *Profile) error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fmt.Errorf("rlimit core: %w", err)
	}
	for _, l := range []struct {
		name     string
		resource int
		value    uint64
	}{{"nofile", unix.RLIMIT_NOFILE, p.NoFile}, {"nproc", unix.RLIMIT_NPROC, p.NProc}} {
		if l.value == 0 {
			continue
		}
		var cur unix.Rlimit
		if err := unix.Getrlimit(l.resource, &cur); err != nil {
			return fmt.Errorf("rlimit %s: %w", l.name, err)
		}
		v := min(l.value, cur.Max)
		if err := unix.Setrlimit(l.resource, &unix.Rlimit{Cur: v, Max: v}); err != nil {
			return fmt.Errorf("rlimit %s: %w", l.name, err)
		}
	}
	if p.Nice > 0 {
		cur, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
		if err != nil {
			return fmt.Errorf("nice: %w", err)
		}
		// Getpriority's raw syscall answers 20-nice on Linux; x/sys
		// returns that raw value there and the nice value on macOS.
		if err := unix.Setpriority(unix.PRIO_PROCESS, 0, niceOf(cur)+p.Nice); err != nil {
			return fmt.Errorf("nice: %w", err)
		}
	}
	return nil
}

// ownGroup puts the process in a process group of its own, so the server
// can signal the plugin and everything it starts at once.
func ownGroup() error {
	if unix.Getpgrp() == unix.Getpid() {
		return nil
	}
	return unix.Setpgid(0, 0)
}
