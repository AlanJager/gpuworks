//go:build !unix

package plugintest

import (
	"os"
	"syscall"
)

// Plugin processes need unix socketpairs (filePair); these only keep the
// package building elsewhere.
func sysProcAttr() *syscall.SysProcAttr { return nil }

func killGroup(p *os.Process) error { return p.Kill() }
