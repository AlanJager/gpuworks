//go:build darwin

package selfinstall

import "syscall"

// exitedProcess reports whether pid has exited but not yet been reaped. A
// zombie still answers signal 0 and keeps its pid, so without this check a
// server that this process started (and has now killed) would look alive until
// the installer itself exits. macOS has no /proc, and its answer is
// wait4(WNOHANG): the call reaps the child it reports, which is the server this
// installer started itself, so reaping it here is exactly the intent. A pid that
// is not this process's child fails with ECHILD and is reaped by its own parent.
func exitedProcess(pid int) bool {
	var status syscall.WaitStatus
	waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
	if err != nil {
		return false
	}
	return waited == pid
}
