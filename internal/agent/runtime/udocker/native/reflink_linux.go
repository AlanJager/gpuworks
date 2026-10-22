package native

import (
	"os"

	"golang.org/x/sys/unix"
)

// ReflinkSupported reports whether dir's filesystem can clone file extents
// (FICLONE), which is what makes a cached base rootfs cheap to reuse.
func ReflinkSupported(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	src, err := os.CreateTemp(dir, ".reflink-probe-src-*")
	if err != nil {
		return false
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.Write([]byte("reflink probe")); err != nil {
		return false
	}
	dst, err := os.CreateTemp(dir, ".reflink-probe-dst-*")
	if err != nil {
		return false
	}
	defer os.Remove(dst.Name())
	defer dst.Close()
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd())) == nil
}
