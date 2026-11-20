//go:build linux && dqlite

package cluster

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// An admitted transaction that reads before it writes must not lose its write
// to a writer outside the admission. The scheduling pool is such a writer: it
// committed between the read and the write, and the write failed at once with
// a snapshot conflict.
func TestNativeAdmittedTransactionOwnsTheWriterFromItsFirstRead(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := db.SQL.ExecContext(ctx, `CREATE TABLE owned_writer (id INTEGER PRIMARY KEY, source TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}

	tx, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var before int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM owned_writer`).Scan(&before); err != nil || before != 0 {
		t.Fatalf("read inside the admitted transaction = %d, %v", before, err)
	}

	outside := make(chan error, 1)
	go func() {
		_, err := db.Scheduling.ExecContext(ctx, `INSERT INTO owned_writer (source) VALUES ('scheduling')`)
		outside <- err
	}()
	select {
	case err := <-outside:
		t.Fatalf("a writer outside the admission committed under an open admitted transaction (err=%v)", err)
	case <-time.After(400 * time.Millisecond):
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO owned_writer (source) VALUES ('admitted')`); err != nil {
		t.Fatalf("write after a read in the admitted transaction: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-outside:
		if err != nil {
			t.Fatalf("the waiting outside writer failed after the admitted commit: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the outside writer never completed after the admitted commit")
	}
	var sources []string
	rows, err := db.SQL.QueryContext(ctx, `SELECT source FROM owned_writer ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var source string
		if err := rows.Scan(&source); err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil || len(sources) != 2 || sources[0] != "admitted" || sources[1] != "scheduling" {
		t.Fatalf("rows = %v, %v; want the admitted write then the outside write", sources, err)
	}
}

// A read-only admitted transaction keeps a plain snapshot and does not hold
// the writer, and an admitted transaction that never writes commits cleanly.
func TestNativeReadOnlyAdmittedTransactionLeavesTheWriterFree(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := db.SQL.ExecContext(ctx, `CREATE TABLE owned_writer (id INTEGER PRIMARY KEY, source TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	reader, err := BeginWriterTransaction(ctx, db.SQL, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := reader.QueryRowContext(ctx, `SELECT count(*) FROM owned_writer`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	quick, stop := context.WithTimeout(ctx, 3*time.Second)
	_, err = db.Scheduling.ExecContext(quick, `INSERT INTO owned_writer (source) VALUES ('scheduling')`)
	stop()
	if err != nil {
		t.Fatalf("a read-only admitted transaction blocked an outside writer: %v", err)
	}
	if err := reader.Commit(); err != nil {
		t.Fatal(err)
	}
	idle, err := BeginWriterTransaction(ctx, db.SQL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := idle.Commit(); err != nil {
		t.Fatalf("an admitted transaction with no statement did not commit: %v", err)
	}
	if err := db.SQL.QueryRowContext(ctx, `SELECT count(*) FROM owned_writer`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rows after the idle commit = %d, %v; want 1", count, err)
	}
}
