//go:build !unix

package plugintest

import (
	"errors"
	"net"
	"os"
)

// socketPair falls back to an in-memory pipe where stream socketpairs do
// not exist; the plugin sees the same HTTP/2 traffic.
func socketPair() (net.Conn, net.Conn, error) {
	a, b := net.Pipe()
	return a, b, nil
}

// filePair needs stream socketpairs, which this system lacks.
func filePair() (net.Conn, *os.File, error) {
	return nil, nil, errors.New("plugintest: running a plugin process needs unix socketpairs")
}
