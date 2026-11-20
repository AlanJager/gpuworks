// Package restorehold records that a completed restore must not schedule until
// the old member is isolated. It is a leaf so install and startup can share the
// marker without importing the database.
package restorehold

import (
	"errors"
	"os"
	"path/filepath"
)

// SchedulingIsolationFile is left in the data directory after a completed
// restore. Startup must keep scheduling closed until the old member is isolated.
const SchedulingIsolationFile = ".scheduling-isolation-required"

// MarkSchedulingIsolationRequired records that a restored process must not
// schedule until the old member is isolated. The directory entry is durable
// across the next process start.
func MarkSchedulingIsolationRequired(dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return err
	}
	marker := filepath.Join(dataDir, SchedulingIsolationFile)
	file, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString("old member must be isolated before scheduling resumes\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDir(dataDir)
}

// SchedulingIsolationRequired reports whether a completed restore still needs
// the old member isolated before scheduling can open.
func SchedulingIsolationRequired(dataDir string) (bool, error) {
	_, err := os.Lstat(filepath.Join(dataDir, SchedulingIsolationFile))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ClearSchedulingIsolationRequired drops the hold after isolation is confirmed
// so a later start is ordinary leadership.
func ClearSchedulingIsolationRequired(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, SchedulingIsolationFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(dataDir)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
