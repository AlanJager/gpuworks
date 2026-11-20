//go:build !linux

package packages

import (
	"errors"
	"os"
)

func renameRootPackageNoReplace(root *os.Root, oldname, newname string) (bool, error) {
	return false, errors.New("catalog package publication through a held root requires Linux rename without replacement")
}
