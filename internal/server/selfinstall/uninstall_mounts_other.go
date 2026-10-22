//go:build !linux && !darwin

package selfinstall

import "errors"

func uninstallMountPoints() ([]string, error) {
	return nil, errors.New("mount inspection is only supported on Linux and macOS")
}
