package backupexclude

import "golang.org/x/sys/unix"

func hasXattr(path string) bool {
	n, err := unix.Getxattr(path, excludeAttr, nil)
	return err == nil && n > 0
}
