//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"
)

func TestNativeWritersWaitForCommit(t *testing.T) {
	requireDatabaseHelper(t)
	for _, local := range []bool{false, true} {
		name := "ordinary"
		if local {
			name = "scheduling"
		}
		t.Run(name, func(t *testing.T) {
			db := contentionDatabase(t)
			writer := db.SQL
			if local {
				writer = db.Scheduling
			}
			held := holdContentionWriter(t, writer)
			done := make(chan error, 1)
			go func() {
				var id int
				done <- db.SQL.QueryRowContext(t.Context(), "INSERT INTO contention VALUES (2) RETURNING id").Scan(&id)
			}()
			select {
			case err := <-done:
				t.Fatalf("waiting writer returned before commit: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := held.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("writer did not resume after commit")
			}
			assertContentionRows(t, db.SQL, 2)
		})
	}
}

func TestNativeWaitingWriterCancellationHasUnknownOutcome(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	held := holdContentionWriter(t, db.Scheduling)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	var id int
	err := db.SQL.QueryRowContext(ctx, "INSERT INTO contention VALUES (2) RETURNING id").Scan(&id)
	deadline, _ := ctx.Deadline()
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, driver.ErrBadConn) || time.Now().Before(deadline) {
		t.Fatalf("canceled waiting writer: err=%v context=%v", err, ctx.Err())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("waiting writer ignored caller deadline")
	}
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	// A queued autocommit may execute before the peer observes wire closure.
	// A deadline proves neither commit nor rollback; this SQL is never replayed.
	// A fresh write also proves the canceled native request released its queue.
	if _, err := db.SQL.ExecContext(t.Context(), "INSERT INTO contention VALUES (3)"); err != nil {
		t.Fatal(err)
	}
	var canceled int
	if err := db.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contention WHERE id=2").Scan(&canceled); err != nil || (canceled != 0 && canceled != 1) {
		t.Fatalf("canceled insert count=%d err=%v", canceled, err)
	}
	assertContentionRows(t, db.SQL, 2+canceled)
	t.Logf("actual uncertain queued autocommit persisted rows=%d; fresh writer succeeded", canceled)
}

func TestNativeWaitingWriterTimeout(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	held := holdContentionWriter(t, db.SQL)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := time.Now()
	var id int
	err := db.SQL.QueryRowContext(ctx, "INSERT INTO contention VALUES (2) RETURNING id").Scan(&id)
	elapsed := time.Since(start)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("native timeout: err=%v context=%v", err, ctx.Err())
	}
	if elapsed < 4*time.Second || elapsed > 8*time.Second {
		t.Fatalf("native wait duration=%v", elapsed)
	}
	if err := held.Commit(); err != nil {
		t.Fatal(err)
	}
	assertContentionRows(t, db.SQL, 1)
}

func contentionDatabase(t *testing.T) *Database {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	db, err := OpenSingleDatabase(ctx, t.TempDir(), "contention.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.SQL.ExecContext(ctx, "CREATE TABLE contention (id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	return db
}

func holdContentionWriter(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO contention VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	return tx
}

func assertContentionRows(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contention").Scan(&count); err != nil || count != want {
		t.Fatalf("rows=%d want=%d err=%v", count, want, err)
	}
}
