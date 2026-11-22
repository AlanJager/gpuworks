//go:build !linux || !dqlite

package cluster

import (
	"context"
	"net"
)

// ReadDatabaseImage requires the native Linux dqlite implementation.
func ReadDatabaseImage(context.Context, string, string, func(context.Context, string) (net.Conn, error)) ([]byte, error) {
	return nil, ErrUnavailable
}
