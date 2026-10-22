//go:build linux

package engine

import "syscall"

// filesystemType reads the filesystem magic statfs(2) reports for name. It is
// the only part of the project quota check that is not portable.
func filesystemType(name string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(name, &stat); err != nil {
		return 0, err
	}
	return int64(stat.Type), nil
}
