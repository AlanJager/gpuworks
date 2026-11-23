package runtime

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// OpenRegularImageFile follows normal path resolution but takes a metadata-only
// handle first. Reopening that handle reads exactly the regular file checked,
// including when a symlink is replaced during preview.
func OpenRegularImageFile(root *os.Root, path string) (*os.File, error) {
	open := os.OpenFile
	if root != nil {
		open = root.OpenFile
	}
	handle, err := open(path, unix.O_PATH, 0)
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrImageFileNotRegular
	}
	return os.Open(fmt.Sprintf("/proc/self/fd/%d", handle.Fd()))
}
