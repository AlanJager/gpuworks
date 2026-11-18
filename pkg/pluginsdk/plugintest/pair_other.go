//go:build !unix

package plugintest

import "net"

// socketPair falls back to an in-memory pipe where stream socketpairs do
// not exist; the plugin sees the same HTTP/2 traffic.
func socketPair() (net.Conn, net.Conn, error) {
	a, b := net.Pipe()
	return a, b, nil
}
