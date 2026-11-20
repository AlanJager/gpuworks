//go:build linux && dqlite

package cluster

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	dqlitedriver "github.com/canonical/go-dqlite/v3/driver"
)

// Forward real native protocol traffic over a deadline-aware pipe, holding only
// server response delivery. SQL execution and its COMMIT/ROLLBACK are real.
type completionReplyHold struct {
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	sent    []byte
}
type completionReplyWire struct {
	net.Conn
	actual net.Conn
}

// Both relay goroutines and the caller own cleanup paths for the same real
// TCP transport. Share its first Close outcome instead of creating fixture-only
// duplicate-close errors when cancellation wakes multiple owners.
type completionActualWire struct {
	net.Conn
	once sync.Once
	err  error
}

func (c *completionActualWire) Close() error {
	c.once.Do(func() { c.err = c.Conn.Close() })
	return c.err
}

func (c *completionReplyWire) Close() error { return errors.Join(c.Conn.Close(), c.actual.Close()) }
func (h *completionReplyHold) dial(ctx context.Context, address string) (net.Conn, error) {
	raw, err := client.DefaultDialFunc(ctx, address)
	if err != nil {
		return nil, err
	}
	actual := &completionActualWire{Conn: raw}
	caller, relay := net.Pipe()
	go func() {
		_, _ = io.Copy(actual, io.TeeReader(relay, completionSentWriter{h}))
		_ = actual.Close()
		_ = relay.Close()
	}()
	go func() {
		defer actual.Close()
		defer relay.Close()
		buf := make([]byte, 65536)
		for {
			n, err := actual.Read(buf)
			if n > 0 {
				if h.armed.Load() {
					h.once.Do(func() { close(h.entered) })
					<-h.release
				}
				if _, e := relay.Write(buf[:n]); e != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return &completionReplyWire{Conn: caller, actual: actual}, nil
}

type completionSentWriter struct{ h *completionReplyHold }

func (w completionSentWriter) Write(p []byte) (int, error) {
	w.h.mu.Lock()
	w.h.sent = append(w.h.sent, p...)
	w.h.mu.Unlock()
	return len(p), nil
}
func (h *completionReplyHold) count(statement string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return bytes.Count(h.sent, []byte(statement))
}
func completionNativeOwner(t *testing.T, db *Database, h *completionReplyHold) *transactionSQLConn {
	t.Helper()
	seeds := client.NewInmemNodeStore()
	if err := seeds.Set(t.Context(), []client.NodeInfo{{ID: db.ID, Address: db.Address}}); err != nil {
		t.Fatal(err)
	}
	native, err := dqlitedriver.New(seeds, dqlitedriver.WithDialFunc(h.dial))
	if err != nil {
		t.Fatal(err)
	}
	connector, err := native.OpenConnector("contention.db")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := connector.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := wrapTransactionSQLConn(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return conn.(*transactionSQLConn)
}
func TestNativeCompletionUsesOriginalBeginDeadline(t *testing.T) {
	requireDatabaseHelper(t)
	for _, statement := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(statement, func(t *testing.T) {
			db := contentionDatabase(t)
			h := &completionReplyHold{entered: make(chan struct{}), release: make(chan struct{})}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(h.release) }) }
			defer release()
			conn := completionNativeOwner(t, db, h)
			defer conn.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 600*time.Millisecond)
			defer cancel()
			tx, err := conn.BeginTx(ctx, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO contention VALUES (71)", nil); err != nil {
				t.Fatal(err)
			}
			h.armed.Store(true)
			finished := make(chan error, 1)
			go func() {
				if statement == "COMMIT" {
					finished <- tx.Commit()
				} else {
					finished <- tx.Rollback()
				}
			}()
			select {
			case <-h.entered:
			case <-time.After(time.Second):
				t.Fatal("native completion produced no actual response")
			}
			var completionErr error
			select {
			case completionErr = <-finished:
			case <-time.After(900 * time.Millisecond):
				t.Error("native completion discarded original BeginTx deadline and retained writer")
				release()
				completionErr = <-finished
			}
			release()
			if !errors.Is(completionErr, context.DeadlineExceeded) || errors.Is(completionErr, driver.ErrBadConn) {
				t.Errorf("completion error=%T %v, want non-replayable context deadline", completionErr, completionErr)
			}
			if conn.IsValid() {
				t.Error("timed out completion retained native connection")
			}
			if n := h.count(statement); n != 1 {
				t.Errorf("sent %s %d times, want one actual completion and no replay", statement, n)
			}
			// The delayed response can hide a committed transaction. Its one real
			// COMMIT is not replayed; an actual ROLLBACK leaves no row.
			var count int
			if err := db.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contention WHERE id=71").Scan(&count); err != nil {
				t.Fatal(err)
			}
			want := 0
			if statement == "COMMIT" {
				want = 1
			}
			if count != want {
				t.Errorf("actual native completion rows=%d want=%d", count, want)
			}
		})
	}
}
func TestNativeCanceledCompletionDiscardsWithoutSendingSQL(t *testing.T) {
	requireDatabaseHelper(t)
	for _, statement := range []string{"COMMIT", "ROLLBACK"} {
		t.Run(statement, func(t *testing.T) {
			db := contentionDatabase(t)
			h := &completionReplyHold{entered: make(chan struct{}), release: make(chan struct{})}
			close(h.release)
			conn := completionNativeOwner(t, db, h)
			defer conn.Close()
			ctx, cancel := context.WithCancel(t.Context())
			tx, err := conn.BeginTx(ctx, driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := conn.ExecContext(ctx, "INSERT INTO contention VALUES (72)", nil); err != nil {
				t.Fatal(err)
			}
			cancel()
			if statement == "COMMIT" {
				err = tx.Commit()
			} else {
				err = tx.Rollback()
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("completion error=%v want canceled", err)
			}
			if conn.IsValid() {
				t.Error("canceled completion retained native wire")
			}
			if n := h.count(statement); n != 0 {
				t.Errorf("already canceled completion sent %s %d times", statement, n)
			}
			if _, err := db.SQL.ExecContext(t.Context(), "INSERT INTO contention VALUES (73)"); err != nil {
				t.Fatal("physical disposal did not release native writer", err)
			}
			var count int
			if err := db.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contention WHERE id=72").Scan(&count); err != nil || count != 0 {
				t.Fatalf("canceled transaction persisted count=%d err=%v", count, err)
			}
		})
	}
}

func TestNativeCompletionCancellationWithoutDeadlineDiscardsWire(t *testing.T) {
	requireDatabaseHelper(t)
	db := contentionDatabase(t)
	h := &completionReplyHold{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(h.release) }) }
	defer release()
	conn := completionNativeOwner(t, db, h)
	defer conn.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tx, err := conn.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO contention VALUES (74)", nil); err != nil {
		t.Fatal(err)
	}
	h.armed.Store(true)
	finished := make(chan error, 1)
	go func() { finished <- tx.Commit() }()
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("no actual native COMMIT response")
	}
	cancel()
	// The COMMIT has executed, but its response is held. Cancellation must
	// discard the wire without replaying this unknown completion outcome.
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("completion lost cancellation: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		release()
		<-finished
		t.Fatal("active COMMIT ignored cancellation without deadline")
	}
	if conn.IsValid() {
		t.Error("canceled completion retained native connection")
	}
	release()
	var count int
	if err := db.SQL.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM contention WHERE id=74").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("unknown COMMIT was replayed or incorrectly treated as rollback: rows=%d", count)
	}
	if _, err := db.SQL.ExecContext(t.Context(), "INSERT INTO contention VALUES (75)"); err != nil {
		t.Fatalf("fresh writer unavailable after canceled completion: %v", err)
	}
	if n := h.count("COMMIT"); n != 1 {
		t.Fatalf("actual COMMIT count=%d", n)
	}
}
