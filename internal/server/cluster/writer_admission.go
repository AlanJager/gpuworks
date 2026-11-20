package cluster

import (
	"context"
	"database/sql"
)

type writerAdmissionKey struct{}

// BeginWriterTransaction queues participating writers before taking a SQL
// connection. The native pool owns the admission, so independently opened
// Store views share it. Raw SQL transactions and other pools are unaffected.
// The native transaction releases admission after actual commit or rollback,
// including database/sql's asynchronous rollback on context cancellation.
func BeginWriterTransaction(ctx context.Context, pool *sql.DB, options *sql.TxOptions) (*sql.Tx, error) {
	owner, ok := pool.Driver().(interface {
		acquireWriter(context.Context) (func(), error)
	})
	if !ok {
		if observed := observeTransactionBegin(ctx, "sql_begin_total"); observed != nil {
			defer observed()
		}
		return pool.BeginTx(ctx, options)
	}
	release, err := owner.acquireWriter(ctx)
	if err != nil {
		return nil, err
	}
	observed := observeTransactionBegin(ctx, "sql_begin_total")
	tx, err := pool.BeginTx(context.WithValue(ctx, writerAdmissionKey{}, release), options)
	if observed != nil {
		observed()
	}
	if err != nil {
		release()
	}
	return tx, err
}
