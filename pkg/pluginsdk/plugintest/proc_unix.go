//go:build unix

package plugintest

import (
	"os"
	"syscall"
)

// killGroup kills a plugin process and every process of its group.
func killGroup(p *os.Process) error { return syscall.Kill(-p.Pid, syscall.SIGKILL) }
