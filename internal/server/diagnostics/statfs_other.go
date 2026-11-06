//go:build !linux && !darwin

package diagnostics

import "errors"

func statfs(string) (int64, int64, error) {
	return 0, 0, errors.New("filesystem usage is not available on this platform")
}

func writableDir(path string) bool { return isDir(path) }
