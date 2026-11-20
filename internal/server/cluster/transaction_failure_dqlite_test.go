//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

func TestNativeFailedCommitDoesNotPoisonNextTransaction(t *testing.T) {
	requireDatabaseHelper(t)
	for _, local := range []bool{false, true} {
		pool := "ordinary"
		if local {
			pool = "scheduling"
		}
		for _, idle := range []int{1, 0} {
			name := "pooled"
			if idle == 0 {
				name = "discarded"
			}
			t.Run(pool+"/"+name, func(t *testing.T) { testNativeFailedCommitDoesNotPoisonNextTransaction(t, idle, local) })
		}
	}
}

func testNativeFailedCommitDoesNotPoisonNextTransaction(t *testing.T, idle int, local bool) {
	db := contentionDatabase(t)
	pool := db.SQL
	if local {
		pool = db.Scheduling
	}
	pool.SetMaxOpenConns(1)
	pool.SetMaxIdleConns(idle)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	failNativeConstraintCommit(t, ctx, conn)
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	// Clean up the deliberately poisoned baseline after reporting RED, so a
	// retained native transaction cannot obscure the actual assertion failure.
	defer func() { _, _ = pool.ExecContext(context.Background(), "ROLLBACK") }()
	next, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("next BEGIN after failed COMMIT: %v", err)
	}
	defer next.Rollback()
	var children int
	if err := next.QueryRowContext(ctx, "SELECT COUNT(*) FROM child").Scan(&children); err != nil || children != 0 {
		t.Fatalf("failed transaction leaked rows: children=%d err=%v", children, err)
	}
	statement, err := next.PrepareContext(ctx, "INSERT INTO parent VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	defer statement.Close()
	if _, err := statement.ExecContext(ctx, 42); err != nil {
		t.Fatal(err)
	}
	rows, err := next.QueryContext(ctx, "SELECT id FROM parent WHERE id=?", 42)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || !slices.Equal(columns, []string{"id"}) {
		t.Fatalf("native row columns=%v err=%v", columns, err)
	}
	var parent int
	if !rows.Next() || rows.Scan(&parent) != nil || parent != 42 || rows.Next() || rows.Err() != nil {
		t.Fatalf("native prepared/query rows: parent=%d err=%v", parent, rows.Err())
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := next.Commit(); err != nil {
		t.Fatalf("next transaction cannot commit: %v", err)
	}
}

func failNativeConstraintCommit(t *testing.T, ctx context.Context, conn *sql.Conn) {
	t.Helper()
	// Deferred constraints fail COMMIT after successful writes, leaving the
	// native SQLite transaction open. This is a deterministic SQL failure, not
	// a transport mock or an unconfirmed transaction replay.
	for _, statement := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	var enabled int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign key enforcement=%d err=%v", enabled, err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO child VALUES (1, 42)"); err != nil {
		t.Fatal(err)
	}
	err = tx.Commit()
	var sqlFailure dqlitedriver.Error
	if !errors.As(err, &sqlFailure) || sqlFailure.Code&0xff != 19 {
		t.Fatalf("expected native COMMIT constraint failure, got %T: %v", err, err)
	}
	if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("database/sql rollback after failed commit = %v", err)
	}
}

func TestNativeFailedCommitPhysicallyClosesOnlyFailedWire(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var closed atomic.Int64
	pool := trackedTransactionPool(t, ctx, db, &closed)
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(2)
	failed, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer failed.Close()
	healthy, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	before := closed.Load()
	failNativeConstraintCommit(t, ctx, failed)
	if err := failed.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	if got := closed.Load() - before; got != 1 {
		t.Fatalf("failed COMMIT physical wire closes=%d, want exactly 1", got)
	}
	var children int
	if err := healthy.QueryRowContext(ctx, "SELECT COUNT(*) FROM child").Scan(&children); err != nil || children != 0 {
		t.Fatalf("healthy connection after discard: children=%d err=%v", children, err)
	}
	if err := healthy.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closed.Load() - before; got != 1 {
		t.Fatalf("healthy wire was discarded too: closes=%d", got)
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO parent VALUES (42)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("healthy wire cannot write after failed COMMIT discard: %v", err)
	}
}

func TestNativeFailedRollbackPhysicallyDiscardsWire(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var closed atomic.Int64
	pool := trackedTransactionPool(t, ctx, db, &closed)
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	before := closed.Load()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO contention VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	// End the native transaction before database/sql knows it has ended, so
	// the driver's subsequent ROLLBACK returns a real SQL failure.
	if _, err := tx.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	err = tx.Rollback()
	var sqlFailure dqlitedriver.Error
	if !errors.As(err, &sqlFailure) || sqlFailure.Code&0xff != 1 {
		t.Fatalf("expected native ROLLBACK SQL error, got %T: %v", err, err)
	}
	if err := conn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	if got := closed.Load() - before; got != 1 {
		t.Fatalf("failed ROLLBACK physical wire closes=%d, want exactly 1", got)
	}
	assertContentionRows(t, pool, 0)
	next, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Rollback(); err != nil {
		t.Fatalf("fresh transaction after ROLLBACK failure: %v", err)
	}
}

func trackedTransactionPool(t *testing.T, ctx context.Context, db *Database, closed *atomic.Int64) *sql.DB {
	t.Helper()
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
		t.Fatal(err)
	}
	native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}
		return &countedTransactionSocket{Conn: conn, closed: closed}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := native.OpenConnector("contention.db")
	if err != nil {
		t.Fatal(err)
	}
	pool := sql.OpenDB(&transactionSQLConnector{Connector: connector})
	t.Cleanup(func() { _ = pool.Close() })
	return pool
}

type countedTransactionSocket struct {
	net.Conn
	closed *atomic.Int64
	once   sync.Once
	err    error
}

func (c *countedTransactionSocket) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.closed.Add(1)
	})
	return c.err
}
