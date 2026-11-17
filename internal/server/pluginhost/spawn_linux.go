//go:build linux

package pluginhost

import "syscall"

// sysProcAttr starts the trampoline in a process group of its own and arms
// the parent-death signal before it runs a single instruction, so a
// server killed outright takes its plugins with it. The signal is tied to
// the forking thread, which spawn keeps locked (see trampolineSpawner).
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
