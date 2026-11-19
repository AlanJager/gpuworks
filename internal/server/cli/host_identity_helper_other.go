//go:build !linux

package cli

import "errors"

func manageHostIdentityHelper(_, _, _ string) error {
	return errors.New("host identity systemd helper is supported only on Linux")
}
