//go:build linux || darwin

package pluginhost

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// socketPair returns the host's end and the plugin's end of a connected
// stream socket pair. Both are close-on-exec; os/exec clears it on the
// plugin's end when it passes the file as an inherited descriptor.
func socketPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return os.NewFile(uintptr(fds[0]), "gwx-host-end"), os.NewFile(uintptr(fds[1]), "gwx-plugin-end"), nil
}

// signalGroup signals the process group the trampoline created.
func signalGroup(pid int, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok || pid <= 0 {
		return fmt.Errorf("cannot signal group %d with %v", pid, sig)
	}
	return syscall.Kill(-pid, s)
}
