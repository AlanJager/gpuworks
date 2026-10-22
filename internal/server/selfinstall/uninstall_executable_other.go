//go:build !linux

package selfinstall

// Other platforms use the process table and reject ambiguous relative serve
// commands. They must not infer an executable path from the operator's cwd.
func uninstallExecutableIdentity(int, string) (ours, known bool) { return false, false }
