//go:build !linux

package hostidentity

import "errors"

// Write is Linux only: on macOS the service reads the facts itself
// (ioreg), and only systemd runs the root step before the service.
func Write(path, group string) error {
	if err := checkTarget(path); err != nil {
		return err
	}
	return errors.New("host-identity --write is supported on Linux only")
}
