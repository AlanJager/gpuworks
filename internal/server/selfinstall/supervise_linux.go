//go:build linux

package selfinstall

import "syscall"

// superviseChildAttr ties the server to its supervisor: a supervisor that is
// killed outright (SIGKILL after a stop timed out) takes the server with it
// instead of leaving it holding the port.
func superviseChildAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
