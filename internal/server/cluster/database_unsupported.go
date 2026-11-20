//go:build !linux || !dqlite

package cluster

import "context"

func StartDatabase(context.Context, DatabaseConfig) (*Database, error) {
	return nil, ErrUnavailable
}
