//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// Hold only the real network connection's Close. Every SQL/protocol operation
// and the deferred-FK COMMIT error come from the pinned SDK and native engine.
type closeContractWire struct {
	net.Conn
	armed      *atomic.Bool
	once       *sync.Once
	entered    chan struct{}
	resume     chan struct{}
	calls      *atomic.Int32
	closeError error
}

func (c *closeContractWire) Close() error {
	if c.armed.Load() {
		c.calls.Add(1)
		c.once.Do(func() { close(c.entered); <-c.resume })
	}
	closeErr := c.Conn.Close()
	if c.armed.Load() {
		return errors.Join(closeErr, c.closeError)
	}
	return closeErr
}

func TestFailedCommitKeepsWriterAdmissionUntilPhysicalClose(t *testing.T) {
	requireDatabaseHelper(t)
	t.Run("original-commit-error", func(t *testing.T) { failedCommitCloseContract(t, nil) })
	t.Run("close-error-preserves-original", func(t *testing.T) {
		failedCommitCloseContract(t, errors.New("observed physical close error after actual socket closure"))
	})
}
func failedCommitCloseContract(t *testing.T, closeError error) {
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	entered, resume := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var closeCalls atomic.Int32
	var closeOnce sync.Once
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(ctx, []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
		t.Fatal(err)
	}
	native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
		wire, err := client.DefaultDialFunc(ctx, address)
		if err != nil {
			return nil, err
		}
		return &closeContractWire{Conn: wire, armed: &armed, once: &closeOnce, entered: entered, resume: resume, calls: &closeCalls, closeError: closeError}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := native.OpenConnector("contention.db")
	if err != nil {
		t.Fatal(err)
	}
	owner := &transactionSQLConnector{Connector: connector, writer: make(chan struct{}, 1)}
	pool := sql.OpenDB(owner)
	pool.SetMaxOpenConns(1)
	var unblock sync.Once
	release := func() { unblock.Do(func() { close(resume) }) }
	defer func() { release(); _ = pool.Close() }()
	// Pin initialization and transaction to the same actual native connection.
	conn, err := pool.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE close_parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE close_child (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES close_parent(id) DEFERRABLE INITIALLY DEFERRED)",
	} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var enabled int
	if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		t.Fatalf("foreign_keys=%d err=%v", enabled, err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	tx, err := BeginWriterTransaction(ctx, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO close_child VALUES (1,99)"); err != nil {
		t.Fatalf("deferred insert must succeed: %v", err)
	}
	// Open and verify the independent observer wire BEFORE the failed COMMIT.
	// Its bounded write must not spend the observation window discovering leader.
	observer := sql.OpenDB(db.SQL.Driver().(*transactionSQLConnector).Connector)
	defer observer.Close()
	observerConn, err := observer.Conn(ctx)
	if err != nil {
		t.Fatalf("pin observer native wire: %v", err)
	}
	defer observerConn.Close()
	var observedRows int
	if err := observerConn.QueryRowContext(ctx, "SELECT COUNT(*) FROM contention").Scan(&observedRows); err != nil {
		t.Fatalf("prewarm observer native SQL: %v", err)
	}
	armed.Store(true)
	committed := make(chan error, 1)
	go func() { committed <- tx.Commit() }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("database/sql never entered physical Close", ctx.Err())
	}
	// Test real shared admission separately from the independent native observer.
	permitCtx, stopPermit := context.WithTimeout(ctx, 100*time.Millisecond)
	permit, err := owner.acquireWriter(permitCtx)
	stopPermit()
	prematurelyReleased := err == nil
	if permit != nil {
		permit()
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected admission waiter error: %v", err)
	}
	observerCtx, stopObserver := context.WithTimeout(ctx, 300*time.Millisecond)
	_, blockedErr := observerConn.ExecContext(observerCtx, "INSERT INTO contention VALUES (11)")
	observerContextErr := observerCtx.Err()
	observerDeadline, _ := observerCtx.Deadline()
	deadlineReached := !time.Now().Before(observerDeadline)
	stopObserver()
	// The SDK applies the context deadline to the socket. Its timeout can
	// return before the context timer goroutine publishes ctx.Err().
	if !errors.Is(blockedErr, context.DeadlineExceeded) || errors.Is(blockedErr, driver.ErrBadConn) || !deadlineReached {
		t.Errorf("pinned native writer did not wait for socket deadline: err=%T %v ctx=%v deadlineReached=%v", blockedErr, blockedErr, observerContextErr, deadlineReached)
	}
	_ = observerConn.Close()
	t.Logf("real native observer before physical Close: %T %v ctx=%v socketDeadlineReached=%v", blockedErr, blockedErr, observerContextErr, deadlineReached)
	release()
	select {
	case err := <-committed:
		if err == nil || errors.Is(err, driver.ErrBadConn) || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
			t.Fatalf("expected genuine deferred FK COMMIT error, got %T %v", err, err)
		}
		var nativeError dqlitedriver.Error
		if !errors.As(err, &nativeError) {
			t.Errorf("native commit error lost: %T %v", err, err)
		}
		if closeError == nil {
			if _, ok := err.(dqlitedriver.Error); !ok {
				t.Errorf("normal native error type changed: %T %v", err, err)
			}
		} else if !errors.Is(err, closeError) {
			t.Errorf("physical close error lost: %v", err)
		}
		t.Logf("real native commit error: %T %v", err, err)
	case <-ctx.Done():
		t.Fatal("actual failed COMMIT lifecycle not joined", ctx.Err())
	}
	if _, err := observer.ExecContext(ctx, "INSERT INTO contention VALUES (12)"); err != nil {
		t.Fatalf("observer after physical Close: %v", err)
	}
	if calls := closeCalls.Load(); calls != 1 {
		t.Errorf("physical wire discarded %d times, want once", calls)
	}
	if prematurelyReleased {
		t.Error("writer admission released before actual failed-transaction wire discard completed")
	}
	var childCount int
	if err := observer.QueryRowContext(ctx, "SELECT COUNT(*) FROM close_child").Scan(&childCount); err != nil || childCount != 0 {
		t.Fatalf("failed deferred commit persisted rows=%d err=%v", childCount, err)
	}
}
