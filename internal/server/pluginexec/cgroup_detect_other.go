//go:build !linux

package pluginexec

// DetectCgroup reports ok=false: cgroups exist only on Linux.
func DetectCgroup(unit string) (root, parent string, ok bool) { return "", "", false }
