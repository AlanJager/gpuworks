package selfinstall

import (
	"fmt"
	"os"
)

// The kernel executable is independent of argv[0], cwd, PATH and aliases.
func uninstallExecutableIdentity(pid int, binary string) (ours, known bool) {
	proc := fmt.Sprintf("/proc/%d/exe", pid)
	path, _ := os.Readlink(proc)
	// Stat the kernel reference as well: an unlinked hardlink retains the
	// inode even though its old pathname no longer resolves.
	if path != "" && sameUninstallExecutable(path, binary) {
		return true, true
	}
	actual, actualErr := os.Stat(proc)
	target, targetErr := os.Stat(binary)
	if actualErr == nil && targetErr == nil {
		return os.SameFile(actual, target), true
	}
	return false, false
}
