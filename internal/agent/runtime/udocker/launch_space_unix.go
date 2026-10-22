//go:build linux || darwin

package udocker

import "syscall"

// freeBytes is the space at path an unprivileged user may still use, or -1
// when it cannot be measured.
func freeBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
