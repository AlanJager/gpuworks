//go:build !linux

package native

// ReflinkSupported is false off Linux: FICLONE is Linux-only.
func ReflinkSupported(string) bool { return false }
