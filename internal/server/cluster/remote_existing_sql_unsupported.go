//go:build !linux || !dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net"
)

func OpenRemoteExistingReadSQL(ctx context.Context, _ Member, _ string, _ func(context.Context, string) (net.Conn, error)) (*sql.DB, func() error, error) {
	return nil, nil, errors.Join(ErrUnavailable, ctx.Err())
}
