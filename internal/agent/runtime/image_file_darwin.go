package runtime

import (
	"os"

	"golang.org/x/sys/unix"
)

// OpenRegularImageFile checks the resolved target before opening it and
// verifies that the opened descriptor still refers to that regular file.
// Darwin has neither Linux O_PATH nor procfs descriptor reopening.
func OpenRegularImageFile(root *os.Root, path string) (*os.File, error) {
	stat, open := os.Stat, os.OpenFile
	if root != nil {
		stat, open = root.Stat, root.OpenFile
	}
	before, err := stat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, ErrImageFileNotRegular
	}
	file, err := open(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		file.Close()
		return nil, ErrImageFileNotRegular
	}
	return file, nil
}
