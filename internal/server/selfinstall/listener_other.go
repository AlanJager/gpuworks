//go:build !linux

package selfinstall

// listenerPID has no /proc to read outside Linux; a held address is then
// described by what answers on it instead.
func listenerPID(string) int { return 0 }
