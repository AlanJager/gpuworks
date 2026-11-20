//go:build !linux || !dqlite

package store

import (
	"context"
	"errors"
)

// ImportRuntimeSQLite requires the explicitly enabled native database backend.
func ImportRuntimeSQLite(context.Context, string, string) (string, error) {
	return "", errors.New("SQLite import requires a Linux dqlite-enabled server")
}
