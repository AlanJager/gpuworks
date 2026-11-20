//go:build linux

package packages

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func renameRootPackageNoReplace(root *os.Root, oldname, newname string) (renamed bool, err error) {
	directory, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	err = unix.Renameat2(int(directory.Fd()), oldname, int(directory.Fd()), newname, unix.RENAME_NOREPLACE)
	return err == nil, err
}
