//go:build linux && cgo && dqlite

package main

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A statement that fails after it has produced a row must report its own
// error. The node used to send the failure behind the rows it had already
// encoded, and a client then read the column count as the error code and the
// first column name as the message: a busy database arrived as error 2 "token".
func TestQueryThatFailsAfterARowReportsItsOwnError(t *testing.T) {
	address := freeAddress(t)
	node := startServe(t, "--dir", dataDir(t), "--address", address)
	defer node.stop(t, syscall.SIGTERM)
	db := openSQL(t, address, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE TABLE numbers (token INTEGER, other INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO numbers VALUES (1, 1), (-9223372036854775808, 2)`); err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]string{
		"prepared": `SELECT abs(token) AS token, other FROM numbers ORDER BY other`,
		"direct":   `SELECT abs(token) AS token, other FROM numbers WHERE other > 0 ORDER BY other`,
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, query)
			if err == nil {
				for rows.Next() {
				}
				err = rows.Err()
				_ = rows.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "integer overflow") {
				t.Fatalf("failed query reported %v, want SQLite's integer overflow", err)
			}
			if strings.Contains(err.Error(), "token") {
				t.Fatalf("failed query reported a column name as its error: %v", err)
			}
			// The connection stays usable after the failure.
			var count int
			if err := db.QueryRowContext(ctx, `SELECT count(*) FROM numbers`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("read after the failure = %d, %v", count, err)
			}
		})
	}
}
