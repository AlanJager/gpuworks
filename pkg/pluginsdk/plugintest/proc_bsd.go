//go:build unix && !linux

package plugintest

import "syscall"

// sysProcAttr puts a plugin process in a group of its own; there is no
// parent-death signal, so the SDK polls for its parent instead.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
