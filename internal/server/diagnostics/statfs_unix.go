//go:build linux || darwin

package diagnostics

import "golang.org/x/sys/unix"

// statfs answers the size of path's filesystem and the room an unprivileged
// writer has left on it (blocks reserved for root not counted).
func statfs(path string) (total, avail int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	block := int64(st.Bsize)
	return int64(st.Blocks) * block, int64(st.Bavail) * block, nil
}

func writableDir(path string) bool {
	return unix.Access(path, unix.W_OK|unix.X_OK) == nil && isDir(path)
}
