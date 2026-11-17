//go:build darwin

package pluginhost

import "syscall"

// sysProcAttr starts the trampoline in a process group of its own. macOS
// has no parent-death signal; a plugin notices the server's death by EOF
// on its descriptors and by polling its parent.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }
