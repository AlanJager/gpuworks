//go:build linux && dqlite

package cli

import (
	"errors"
	"fmt"
	"net"
)

// requireLocalDatabaseAddress fails the install command before any identity is
// written. The advertised database HOST:PORT must be an address on this host,
// and nothing else may already hold the port. The listener is closed before
// return; this does not start the database.
func requireLocalDatabaseAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return errors.New("database address requires host and port")
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || !localDatabaseIP(ip) {
		return fmt.Errorf("database address %s is not a local address", address)
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), port))
	if err != nil {
		return fmt.Errorf("database address %s cannot be bound: %w", address, err)
	}
	return listener.Close()
}

func localDatabaseIP(ip net.IP) bool {
	if ip.IsLoopback() {
		return true
	}
	assigned, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range assigned {
		network, ok := address.(*net.IPNet)
		if ok && network.IP.Equal(ip) {
			return true
		}
	}
	return false
}
