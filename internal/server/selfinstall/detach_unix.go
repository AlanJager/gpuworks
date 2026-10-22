//go:build linux || darwin

package selfinstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// startDetached launches a process that must survive the installer: it gets its
// own session (so a Ctrl-C in the installer's terminal does not reach it) and
// its stdio is /dev/null (the server writes its own rotating log file).
func startDetached(_ context.Context, cmd Command) (int, error) {
	if cmd.Sudo {
		return 0, fmt.Errorf("detached start does not support sudo: %s", cmd)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer devNull.Close()
	c := exec.Command(cmd.Path, cmd.Args...)
	c.Dir = cmd.Dir
	c.Env = append(os.Environ(), cmd.Env...)
	c.Stdin = devNull
	c.Stdout = devNull
	c.Stderr = devNull
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		return 0, err
	}
	pid := c.Process.Pid
	// Release stops the process from being reaped by this one, so it keeps
	// running after the installer exits.
	_ = c.Process.Release()
	return pid, nil
}
