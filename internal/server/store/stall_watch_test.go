package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// startStallWatch runs a process stall watch with a short threshold for the
// test and returns where its reports go.
func startStallWatch(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stalls")
	w := stallwatch.New(stallwatch.Options{Threshold: 100 * time.Millisecond, Dir: dir})
	stallwatch.Use(w)
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.Run(ctx) }()
	t.Cleanup(func() {
		stallwatch.Use(nil)
		cancel()
		<-stopped
	})
	return dir
}

// stallReport waits for a report with every one of want in it.
func stallReport(t *testing.T, dir string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		last := ""
		for _, path := range stallwatch.Reports(dir) {
			raw, _ := os.ReadFile(path)
			last = string(raw)
			missing := false
			for _, w := range want {
				missing = missing || !strings.Contains(last, w)
			}
			if !missing {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no stall report with %q; last report:\n%s", want, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Another connection holds the database's write lock, as a write that waits
// for the disk does. A write through the store then waits in SQLite itself:
// the stall watch must name the wait and keep the stack of the goroutine in
// it.
func TestAWriteWaitingForTheDatabaseIsInAStallReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("uuid-quick", HashToken("token-quick"), "host-quick"); err != nil {
		t.Fatal(err)
	}
	// The watch starts after the set-up: on a starved runner an ordinary
	// statement can take longer than this test's 100ms threshold, and that
	// a wait under the threshold leaves nothing is the watch's own test
	// (stallwatch.TestWaitsUnderTheThresholdLeaveNothing), on a clock.
	dir := startStallWatch(t)

	other, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	lock, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- st.db.Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE nodes SET hostname = 'host-late' WHERE uuid = 'uuid-quick'").Error
		})
	}()
	report := stallReport(t, dir, "goroutines:")
	for _, want := range []string{"  " + StallSiteStatement + "\n", "store.(*watchedPool).ExecContext", "store.TestAWriteWaitingForTheDatabaseIsInAStallReport.func"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if _, err := lock.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the write after the lock was released: %v", err)
	}
	stallReport(t, dir, "ended:")
}

// slowCommits is a database whose transactions commit only when released: a
// commit that waits for the log to reach the disk.
type slowCommits struct {
	*sql.DB
	begun, release chan struct{}
}

func (p *slowCommits) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.DB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &slowCommit{Tx: tx, pool: p}, nil
}

func (p *slowCommits) GetDBConn() (*sql.DB, error) { return p.DB, nil }

type slowCommit struct {
	*sql.Tx
	pool *slowCommits
}

func (t *slowCommit) Commit() error {
	t.pool.begun <- struct{}{}
	<-t.pool.release
	return t.Tx.Commit()
}

// GORM commits outside its statement callbacks. A commit that does not come
// back, and a transaction that waits behind it for the single writer, must
// both be in the report, each under its own name.
func TestACommitThatDoesNotReturnAndTheWriteBehindItAreInAStallReport(t *testing.T) {
	st := newTestStore(t)
	writer, err := st.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	slow := &slowCommits{DB: writer, begun: make(chan struct{}, 1), release: make(chan struct{})}
	st.db.ConnPool = slow
	st.db.Statement.ConnPool = slow
	watchDatabase(st.db)
	dir := startStallWatch(t)

	committed := make(chan error, 1)
	go func() {
		committed <- st.db.Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE settings SET value = value").Error
		})
	}()
	<-slow.begun
	behind := make(chan error, 1)
	go func() {
		behind <- st.db.Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE settings SET value = value").Error
		})
	}()
	report := stallReport(t, dir, "goroutines:", "store.(*watchedPool).BeginTx")
	for _, want := range []string{"  " + StallSiteCommit + "\n", "store.(*watchedTx).Commit", "store.(*slowCommit).Commit"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	close(slow.release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	<-slow.begun
	if err := <-behind; err != nil {
		t.Fatal(err)
	}
	stallReport(t, dir, "ended:")
}

// The store asks whether it holds the database's own pool or a transaction;
// watching the pool must not change the answer.
func TestTheWatchedPoolIsStillSeenAsTheDatabasesPool(t *testing.T) {
	st := newTestStore(t)
	if !isDatabasePool(st.db.Statement.ConnPool) {
		t.Fatal("the store's pool is not seen as the database's pool")
	}
	if db, err := st.db.DB(); err != nil || db == nil {
		t.Fatalf("DB() through the watched pool: %v, %v", db, err)
	}
	err := st.db.Transaction(func(tx *gorm.DB) error {
		if isDatabasePool(tx.Statement.ConnPool) {
			t.Error("a transaction is seen as the database's pool")
		}
		return tx.Transaction(func(inner *gorm.DB) error { return inner.Exec("SELECT 1").Error })
	})
	if err != nil {
		t.Fatalf("nested transaction through the watched pool: %v", err)
	}
}

// A lookup that holds the writer's one connection across its statements
// (withCards) waits for that connection when a write has it. That wait is in
// database/sql, before any statement: the watch must see it too, and the
// statements run on the held connection afterwards.
func TestWaitingForTheWritersConnectionIsInAStallReport(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("uuid-held", HashToken("token-held"), "host-held")
	if err != nil {
		t.Fatal(err)
	}
	dir := startStallWatch(t)

	pool, err := st.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	held, err := pool.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_ = held.Close()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := st.NodeByID(node.ID)
		done <- err
	}()
	report := stallReport(t, dir, "goroutines:")
	for _, want := range []string{"  " + StallSiteConnection + "\n", "store.watchedConnection", "store.(*Store).NodeByID"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	released = true
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the lookup once the connection was free: %v", err)
	}
	stallReport(t, dir, "ended:")

	// On the held connection the statements are the watch's as well.
	err = watchedConnection(st.db, func(tx *gorm.DB) error {
		if _, watched := tx.Statement.ConnPool.(*watchedPool); !watched {
			t.Errorf("statements on the held connection run on %T, which the watch does not see", tx.Statement.ConnPool)
		}
		if isDatabasePool(tx.Statement.ConnPool) {
			t.Error("a held connection is seen as the database's pool")
		}
		return tx.Transaction(func(inner *gorm.DB) error { return inner.Exec("UPDATE nodes SET hostname = hostname").Error })
	})
	if err != nil {
		t.Fatalf("a transaction on the held connection: %v", err)
	}
}

// idleWatch installs a stall watch that nothing runs: no goroutine looks,
// no clock matters. What it holds is exactly the waits that began and did
// not end.
func idleWatch(t *testing.T) *stallwatch.Watchdog {
	t.Helper()
	w := stallwatch.New(stallwatch.Options{Threshold: time.Hour})
	stallwatch.Use(w)
	t.Cleanup(func() { stallwatch.Use(nil) })
	return w
}

// leftOpen fails the test when a call that has returned left its wait open:
// such a wait would be reported as a stall one threshold later, and for
// ever after as one that never returned.
func leftOpen(t *testing.T, w *stallwatch.Watchdog, after string) {
	t.Helper()
	if open := w.Open(); len(open) != 0 {
		t.Fatalf("after %s the watch still holds %+v", after, open)
	}
}

// exerciseWatchedPool calls every method of the watched pool, of a
// transaction begun on it and of a held connection once, each of them
// returning, failing or panicking, and checks after each that it ended its
// wait. began counts the brackets, so a method that stopped being watched
// fails the test too.
func exerciseWatchedPool(t *testing.T, w *stallwatch.Watchdog, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	pool, ok := db.Statement.ConnPool.(*watchedPool)
	if !ok {
		t.Fatalf("the database's pool is %T, which the stall watch does not see", db.Statement.ConnPool)
	}
	if _, err := pool.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS stall_watch_fixture (id INTEGER PRIMARY KEY, name TEXT)"); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "ExecContext")
	if _, err := pool.ExecContext(ctx, "INSERT INTO no_such_table VALUES (1)"); err == nil {
		t.Fatal("a statement on a missing table succeeded")
	}
	leftOpen(t, w, "an ExecContext that failed")
	rows, err := pool.QueryContext(ctx, "SELECT id FROM stall_watch_fixture")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	leftOpen(t, w, "QueryContext")
	var n int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM stall_watch_fixture").Scan(&n); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "QueryRowContext")
	stmt, err := pool.PrepareContext(ctx, "SELECT id FROM stall_watch_fixture WHERE id = ?")
	if err != nil {
		t.Fatal(err)
	}
	_ = stmt.Close()
	leftOpen(t, w, "PrepareContext")

	// A transaction that commits, one that rolls back on an error, one that
	// rolls back on a panic, and one nested in another (a savepoint).
	if err := db.Transaction(func(tx *gorm.DB) error {
		if _, watched := tx.Statement.ConnPool.(*watchedTx); !watched {
			t.Errorf("statements in a transaction run on %T, which the watch does not see", tx.Statement.ConnPool)
		}
		if err := tx.Exec("INSERT INTO stall_watch_fixture (name) VALUES ('committed')").Error; err != nil {
			return err
		}
		return tx.Transaction(func(inner *gorm.DB) error {
			return inner.Exec("INSERT INTO stall_watch_fixture (name) VALUES ('nested')").Error
		})
	}); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "a committed transaction")
	failed := errors.New("the transaction's own failure")
	if err := db.Transaction(func(tx *gorm.DB) error {
		_ = tx.Exec("INSERT INTO stall_watch_fixture (name) VALUES ('rolled back')").Error
		return failed
	}); !errors.Is(err, failed) {
		t.Fatalf("a transaction that failed returned %v", err)
	}
	leftOpen(t, w, "a rolled back transaction")
	func() {
		defer func() { _ = recover() }()
		_ = db.Transaction(func(tx *gorm.DB) error { panic("inside a transaction") })
	}()
	leftOpen(t, w, "a transaction that panicked")
	manual := db.Begin()
	if manual.Error != nil {
		t.Fatal(manual.Error)
	}
	if err := manual.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "Begin and Rollback")
	if err := db.Raw("SELECT COUNT(*) FROM stall_watch_fixture").Scan(&n).Error; err != nil || n != 2 {
		t.Fatalf("rows after the transactions: %d, %v; want the committed and the nested one", n, err)
	}
	leftOpen(t, w, "a query through GORM")
}

// Every call into the watched pool ends the wait it began, whether it
// returns, fails or panics. Nothing here depends on time.
func TestEveryWatchedCallEndsItsWait(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("uuid-ends", HashToken("token-ends"), "host-ends")
	if err != nil {
		t.Fatal(err)
	}
	w := idleWatch(t)
	exerciseWatchedPool(t, w, st.db)

	// The held writer connection: its wait for the connection ends when it
	// has it, and when taking it fails or what runs on it panics.
	if err := watchedConnection(st.db, func(tx *gorm.DB) error {
		if open := w.Open(); len(open) != 0 {
			t.Errorf("with the connection held the watch still waits for it: %+v", open)
		}
		return tx.Exec("UPDATE nodes SET hostname = hostname").Error
	}); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "watchedConnection")
	failed := errors.New("what ran on the connection failed")
	if err := watchedConnection(st.db, func(*gorm.DB) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("watchedConnection returned %v", err)
	}
	leftOpen(t, w, "a watchedConnection whose function failed")
	func() {
		defer func() { _ = recover() }()
		_ = watchedConnection(st.db, func(*gorm.DB) error { panic("on the held connection") })
	}()
	leftOpen(t, w, "a watchedConnection whose function panicked")
	if _, err := st.NodeByID(node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.NodeByUUID("uuid-ends"); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "the lookups that hold the writer's connection")

	// A connection that cannot be had: the function never runs, and the
	// wait for the connection still ends.
	closed, err := Open(filepath.Join(t.TempDir(), "closed.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := watchedConnection(closed.db, func(*gorm.DB) error { ran = true; return nil }); err == nil || ran {
		t.Fatalf("a connection of a closed database: err=%v, function ran=%v", err, ran)
	}
	leftOpen(t, w, "a watchedConnection that got no connection")

	// The read pool is watched as the writer is.
	if reader := st.reader(); reader != st.db {
		if _, watched := reader.Statement.ConnPool.(*watchedPool); !watched {
			t.Fatalf("the read pool is %T, which the stall watch does not see", reader.Statement.ConnPool)
		}
		var count int64
		if err := reader.Model(&Node{}).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		leftOpen(t, w, "a query on the read pool")
	}
}
