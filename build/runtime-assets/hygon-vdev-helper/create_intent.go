package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const createIntentName = "vdev-create-pending"

// The marker records only that native creation may have happened. A malformed
// driver config can make its ID unknowable, so an ownership record cannot
// safely represent this state. Managed retries must not allocate again.
func beginCreateIntent(recordPath string) (string, error) {
	if recordPath == "" {
		return "", nil
	}
	if _, err := os.Lstat(recordPath); err == nil {
		return "", fmt.Errorf("ownership record already exists at %s; inspect the recorded device instead of creating another", recordPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("check vdev ownership record before native creation: %w", err)
	}
	path := filepath.Join(filepath.Dir(recordPath), createIntentName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("pending Hygon vdev creation at %s; manual inspection required before retry", path)
		}
		return "", fmt.Errorf("create durable vdev intent before native creation: %w", err)
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return "", fmt.Errorf("sync vdev creation intent %s; native creation was not attempted: %w", path, err)
	}
	if err := syncIntentDirectory(filepath.Dir(path)); err != nil {
		return "", fmt.Errorf("persist vdev creation intent %s; native creation was not attempted: %w", path, err)
	}
	return path, nil
}

// Call only after ownership is durable or native destruction is verified.
func clearCreateIntent(path string) error {
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove completed vdev creation intent %s: %w", path, err)
	}
	return syncIntentDirectory(filepath.Dir(path))
}

func syncIntentDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
