package plugintest

import "syscall"

// sysProcAttr puts a plugin process in a group of its own, killed with the
// process that started it, as the server's trampoline does.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}
