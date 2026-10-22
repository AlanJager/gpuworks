//go:build linux || darwin

package native

import "syscall"

// availableBytes reports the free space usable at path, or -1 when it cannot be
// determined (including when path does not exist).
func availableBytes(path string) int64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return -1
	}
	return int64(st.Bavail) * int64(st.Bsize)
}
