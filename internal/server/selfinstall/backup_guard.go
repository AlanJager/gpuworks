package selfinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// RestorePendingFile blocks startup after an interrupted multi-root restore.
// It is under DataDir even when a custom database path lives elsewhere.
const RestorePendingFile = ".restore-in-progress"

// RestorePending refuses startup until an interrupted restore is inspected.
// The marker must not be deleted merely to make a mixed-generation tree start.
func RestorePending(dataDir string) error {
	path := filepath.Join(dataDir, RestorePendingFile)
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect restore marker: %w", err)
	}
	return fmt.Errorf("incomplete backup restore; inspect recovery state at %s before starting", path)
}
