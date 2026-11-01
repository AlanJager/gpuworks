//go:build unix

package gpu

import (
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd as the leader of its own process group and
// makes its cancellation kill the whole group. A vendor CLI or a runtime
// check is often a wrapper (a shell script, python spawning a helper), and
// killing only the direct child would leave the helper holding the driver
// and the output pipes after the call has given up on it.
func killGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// A negative pid signals the group; the leader's pid is its id.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
