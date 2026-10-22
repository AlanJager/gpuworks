//go:build darwin

package selfinstall

import "syscall"

// superviseChildAttr has no parent-death signal to set on macOS.
func superviseChildAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }
