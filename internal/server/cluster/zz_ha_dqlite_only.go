//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"sync"
)

type quorumSQLSocketKey struct{}

// Ordinary scheduling calls have no owner in their context and keep the
// original dial behavior. Dedicated snapshot connectors opt into this owner.
func dialQuorumSQLSocket(ctx context.Context, address string, dial func(context.Context, string) (net.Conn, error)) (net.Conn, error) {
	if owner, ok := ctx.Value(quorumSQLSocketKey{}).(*quorumSQLSockets); ok {
		return owner.dial(ctx, address, dial)
	}
	return dial(ctx, address)
}

type quorumSQLConnector struct {
	driver.Connector
	owner *quorumSQLSockets
}

func (c *quorumSQLConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return c.Connector.Connect(context.WithValue(ctx, quorumSQLSocketKey{}, c.owner))
}

// Every operation receives a fresh connector, so a protocol donated to the
// SDK's connector cache cannot later be reused by the normal scheduling pool.
func openQuorumSQL(ctx context.Context, connector driver.Connector) (*sql.DB, func() error, error) {
	if connector == nil {
		return nil, nil, errors.New("quorum SQL requires a private local connector")
	}
	life, cancel := context.WithCancel(ctx)
	owner := &quorumSQLSockets{life: life, cancel: cancel}
	pool := sql.OpenDB(&quorumSQLConnector{Connector: connector, owner: owner})
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(0)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = owner.Close(); close(done) })
	var once sync.Once
	var result error
	cleanup := func() error {
		once.Do(func() {
			if !stop() {
				<-done
			}
			result = errors.Join(owner.Close(), pool.Close())
		})
		return result
	}
	return pool, cleanup, nil
}
