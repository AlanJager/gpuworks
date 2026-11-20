//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"sync/atomic"

	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// openTransactionSQL wraps a native connector so failed transaction
// completion discards its wire. Only the ordinary pool queues writers.
func openTransactionSQL(connector driver.Connector, admission bool) *sql.DB {
	owner := &transactionSQLConnector{Connector: connector}
	if admission {
		owner.writer = make(chan struct{}, 1)
	}
	return sql.OpenDB(owner)
}

type transactionSQLConnector struct {
	driver.Connector
	writer chan struct{}
}

// Returning this per-pool owner preserves admission across independent Store
// bindings without a global pool registry or a separate Store lifetime.
func (c *transactionSQLConnector) Driver() driver.Driver { return c }

func (c *transactionSQLConnector) Open(name string) (driver.Conn, error) {
	return wrapTransactionSQLConn(c.Connector.Driver().Open(name))
}

func (c *transactionSQLConnector) acquireWriter(ctx context.Context) (func(), error) {
	if c.writer != nil {
		if observed := observeTransactionBegin(ctx, "admission_queue"); observed != nil {
			defer observed()
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.writer == nil {
		// The local-only scheduling connector deliberately has no admission.
		return func() {}, nil
	}
	select {
	case c.writer <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	var once sync.Once
	release := func() { once.Do(func() { <-c.writer }) }
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (c *transactionSQLConnector) Connect(ctx context.Context) (driver.Conn, error) {
	return wrapTransactionSQLConn(c.Connector.Connect(ctx))
}

func wrapTransactionSQLConn(conn driver.Conn, err error) (driver.Conn, error) {
	if err != nil {
		return nil, err
	}
	native, ok := conn.(*dqlitedriver.Conn)
	if !ok {
		return nil, errors.Join(errors.New("transaction SQL requires a native dqlite connection"), conn.Close())
	}
	return &transactionSQLConn{Conn: native}, nil
}

// Embedding the concrete SDK connection preserves its optional execution,
// preparation, and row interfaces. Only transaction completion is wrapped.
type transactionSQLConn struct {
	*dqlitedriver.Conn
	invalid atomic.Bool
}

func (c *transactionSQLConn) IsValid() bool { return !c.invalid.Load() }

// database/sql serializes driver calls on this connection. Failed completion
// discards its wire before writer admission is released; the later pool close
// must not close the same SDK connection again.
func (c *transactionSQLConn) Close() error {
	if !c.invalid.CompareAndSwap(false, true) {
		return nil
	}
	return c.Conn.Close()
}

func (c *transactionSQLConn) Begin() (driver.Tx, error) {
	if !c.IsValid() {
		return nil, driver.ErrBadConn
	}
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	return &transactionSQLTx{Tx: tx, conn: c}, nil
}

func (c *transactionSQLConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	if !c.IsValid() {
		return nil, driver.ErrBadConn
	}
	release, _ := ctx.Value(writerAdmissionKey{}).(func())
	observed := observeTransactionBegin(ctx, "driver_begin")
	var tx driver.Tx
	var err error
	if release != nil && !options.ReadOnly {
		tx, err = c.beginOwningWriter(ctx)
	} else {
		tx, err = c.Conn.BeginTx(ctx, options)
	}
	if observed != nil {
		observed()
	}
	if err != nil {
		return nil, err
	}
	return &transactionSQLTx{Tx: tx, conn: c, releaseWriter: release, ctx: ctx}, nil
}

// beginOwningWriter starts an admitted transaction that holds the database
// write lock from its first statement. A deferred transaction takes a read
// snapshot on its first SELECT; if a writer outside this admission (the
// local scheduling pool, or a statement run outside a transaction) commits
// before the transaction's first write, that write fails at once with a
// snapshot conflict that no busy timeout can wait out. Owning the writer up
// front makes the other writer wait instead.
func (c *transactionSQLConn) beginOwningWriter(ctx context.Context) (driver.Tx, error) {
	if _, err := c.Conn.ExecContext(ctx, "BEGIN IMMEDIATE", nil); err != nil {
		return nil, err
	}
	return ownedWriterTx{conn: c.Conn}, nil
}

// ownedWriterTx completes a transaction begun by beginOwningWriter when its
// owner has no lifetime context, as the SDK transaction does.
type ownedWriterTx struct{ conn *dqlitedriver.Conn }

func (tx ownedWriterTx) Commit() error {
	_, err := tx.conn.ExecContext(context.Background(), "COMMIT", nil)
	return err
}

func (tx ownedWriterTx) Rollback() error {
	_, err := tx.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	return err
}

type transactionSQLTx struct {
	driver.Tx
	conn          *transactionSQLConn
	releaseWriter func()
	ctx           context.Context
}

func (tx *transactionSQLTx) Commit() error {
	return tx.complete("COMMIT", tx.Tx.Commit)
}

func (tx *transactionSQLTx) Rollback() error {
	return tx.complete("ROLLBACK", tx.Tx.Rollback)
}

func (tx *transactionSQLTx) complete(statement string, legacy func() error) error {
	if tx.releaseWriter != nil {
		defer tx.releaseWriter()
	}
	var err error
	if tx.ctx == nil {
		// Begin has no lifetime context; retain the SDK compatibility path.
		err = legacy()
	} else if err = tx.ctx.Err(); err == nil {
		// The pinned SDK Tx methods replace the BeginTx context with
		// Background. Send their same single statement through the native
		// connection so its original deadline bounds the response read.
		// The patched SDK also interrupts an in-flight call on context cancellation.
		_, err = tx.conn.Conn.ExecContext(tx.ctx, statement, nil)
	}
	if err != nil {
		// A failed or canceled completion may leave the native transaction
		// open. Discard its wire before releasing admission; never replay SQL.
		if closeErr := tx.conn.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}
	return err
}

var (
	_ driver.Validator          = (*transactionSQLConn)(nil)
	_ driver.ConnBeginTx        = (*transactionSQLConn)(nil)
	_ driver.ConnPrepareContext = (*transactionSQLConn)(nil)
	_ driver.ExecerContext      = (*transactionSQLConn)(nil)
	_ driver.QueryerContext     = (*transactionSQLConn)(nil)
	_ driver.Execer             = (*transactionSQLConn)(nil)
	_ driver.Queryer            = (*transactionSQLConn)(nil)
)
