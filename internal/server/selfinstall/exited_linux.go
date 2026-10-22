//go:build linux

package selfinstall

import (
	"fmt"
	"os"
	"strings"
)

// exitedProcess reports whether pid has exited but not yet been reaped. A
// zombie still answers signal 0 and keeps its pid, so without this check a
// server that this process started (and has now killed) would look alive until
// the installer itself exits. Linux exposes the state as the "Z" field of
// /proc/<pid>/stat, which answers without reaping anything.
func exitedProcess(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// The command name is parenthesised and may itself contain spaces and
	// parentheses, so the state is the first field after the last ')'.
	_, rest, ok := strings.Cut(string(raw), ")")
	if !ok {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(rest), "Z")
}
