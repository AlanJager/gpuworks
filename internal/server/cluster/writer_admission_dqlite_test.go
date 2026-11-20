//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

func TestNativeWriterAdmissionCancellationAndBeginFailure(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	tx, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	before := db.SQL.Stats()
	waitCtx, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err = BeginWriterTransaction(waitCtx, db.SQL, nil)
	stop()
	after := db.SQL.Stats()
	if !errors.Is(err, context.DeadlineExceeded) || after.InUse != before.InUse || after.WaitCount != before.WaitCount {
		t.Fatalf("canceled admission consumed a connection: err=%v before=%+v after=%+v", err, before, after)
	}
	other := contentionDatabase(t)
	independent, err := BeginWriterTransaction(ctx, other.SQL, nil)
	if err != nil {
		t.Fatalf("independent pool shared writer admission: %v", err)
	}
	if err := independent.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The scheduling pool does not queue on the admission. A read-only
	// transaction holds the admission without the database writer, which an
	// ordinary admitted transaction now owns from its begin.
	holder, err := BeginWriterTransaction(ctx, db.SQL, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	local, err := BeginWriterTransaction(ctx, db.Scheduling, nil)
	if err != nil {
		t.Fatalf("nonparticipating scheduling pool became blocked: %v", err)
	}
	if err := local.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	// A closed test pool has the exact same connector/admission owner. A
	// failed BEGIN must not prevent the original live pool's next transaction.
	closed := sql.OpenDB(db.SQL.Driver().(*transactionSQLConnector))
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginWriterTransaction(ctx, closed, nil); err == nil {
		t.Fatal("closed pool unexpectedly began a transaction")
	}
	next, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatalf("BEGIN failure retained admission: %v", err)
	}
	if err := next.Rollback(); err != nil {
		t.Fatal(err)
	}
}

// Completion now keeps the BeginTx context rather than delegating the SDK's
// Background-only Tx methods. Hold actual native response delivery or physical
// Close; this proves the permit contract without wrapping an unused Tx method.
func TestNativeWriterAdmissionWaitsForActualCompletion(t *testing.T) {
	requireDatabaseHelper(t)
	for _, mode := range []string{"commit", "rollback", "automatic-rollback", "unknown-commit"} {
		t.Run(mode, func(t *testing.T) {
			db := contentionDatabase(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			h := &completionReplyHold{entered: make(chan struct{}), release: make(chan struct{})}
			var responseOnce sync.Once
			releaseResponse := func() { responseOnce.Do(func() { close(h.release) }) }
			defer releaseResponse()
			var closeArmed atomic.Bool
			var closeOnce sync.Once
			var closeCalls atomic.Int32
			closeEntered, closeResume := make(chan struct{}), make(chan struct{})
			var closeReleaseOnce sync.Once
			releaseClose := func() { closeReleaseOnce.Do(func() { close(closeResume) }) }
			defer releaseClose()
			seeds := client.NewInmemNodeStore()
			if err := seeds.Set(ctx, []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
				t.Fatal(err)
			}
			native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
				conn, err := h.dial(ctx, address)
				if err != nil {
					return nil, err
				}
				return &closeContractWire{Conn: conn, armed: &closeArmed, once: &closeOnce, entered: closeEntered, resume: closeResume, calls: &closeCalls}, nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			connector, err := native.OpenConnector("contention.db")
			if err != nil {
				t.Fatal(err)
			}
			owner := &transactionSQLConnector{Connector: connector, writer: db.SQL.Driver().(*transactionSQLConnector).writer}
			pool := sql.OpenDB(owner)
			defer func() { releaseClose(); releaseResponse(); _ = pool.Close() }()
			activeCtx, stop := context.WithCancel(ctx)
			if mode == "unknown-commit" {
				stop()
				activeCtx, stop = context.WithTimeout(ctx, 650*time.Millisecond)
			}
			defer stop()
			tx, err := BeginWriterTransaction(activeCtx, pool, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(activeCtx, "INSERT INTO contention VALUES (9)"); err != nil {
				t.Fatal(err)
			}
			entered := h.entered
			done := make(chan error, 1)
			if mode == "automatic-rollback" {
				closeArmed.Store(true)
				entered = closeEntered
				stop()
			} else {
				h.armed.Store(true)
				if mode == "rollback" {
					go func() { done <- tx.Rollback() }()
				} else {
					go func() { done <- tx.Commit() }()
				}
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("native completion did not reach actual wire barrier", ctx.Err())
			}
			before := db.SQL.Stats()
			waitCtx, stopWait := context.WithTimeout(ctx, 100*time.Millisecond)
			_, err = BeginWriterTransaction(waitCtx, db.SQL, nil)
			stopWait()
			after := db.SQL.Stats()
			if !errors.Is(err, context.DeadlineExceeded) || after.InUse != before.InUse || after.WaitCount != before.WaitCount {
				t.Fatalf("admission released before actual %s: err=%v before=%+v after=%+v", mode, err, before, after)
			}
			if mode != "unknown-commit" {
				releaseResponse()
			}
			releaseClose()
			if mode != "automatic-rollback" {
				select {
				case err := <-done:
					if mode == "unknown-commit" {
						if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) {
							t.Fatalf("withheld real commit acknowledgement error=%v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("native completion did not finish", ctx.Err())
				}
			}
			releaseResponse()
			next, err := BeginWriterTransaction(ctx, db.SQL, nil)
			if err != nil {
				t.Fatal("completed transaction retained admission", err)
			}
			defer next.Rollback()
			var count int
			if err := next.QueryRowContext(ctx, "SELECT COUNT(*) FROM contention WHERE id=9").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "commit" || mode == "unknown-commit" {
				want = 1
			}
			if count != want {
				t.Fatalf("native outcome rows=%d want%d", count, want)
			}
			statement := "COMMIT"
			if mode == "rollback" || mode == "automatic-rollback" {
				statement = "ROLLBACK"
			}
			expected := 1
			if mode == "automatic-rollback" {
				expected = 0
			}
			if count := h.count(statement); count != expected {
				t.Fatalf("native %s sent%d want%d", statement, count, expected)
			}
			if mode == "automatic-rollback" && closeCalls.Load() != 1 {
				t.Fatal("automatic canceled rollback did not discard actual connection exactly once")
			}
			if err := next.Rollback(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
