package app

import (
	"errors"
	"os"
	"path/filepath"
)

// copyCanonicalPath resolves existing parents too when the destination has
// not been created. Aliases must share both overlap checks and the writer lock.
func copyCanonicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = copyCanonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}
