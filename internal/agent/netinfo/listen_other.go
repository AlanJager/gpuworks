//go:build !linux

package netinfo

import "errors"

// dumpTCPListeners needs Linux sock_diag; GPU nodes run Linux.
func dumpTCPListeners() ([]int, error) {
	return nil, errors.New("sock_diag: listening sockets are only read on linux")
}
