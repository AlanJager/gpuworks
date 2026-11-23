package store

import (
	"context"
	"database/sql"
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
// it, for a plain statement and for a transaction alike. Statements that
// return in time leave nothing.
func TestAWriteWaitingForTheDatabaseIsInAStallReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := Open(path, bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	dir := startStallWatch(t)

	if _, err := st.RegisterNode("uuid-quick", HashToken("token-quick"), "host-quick"); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Transaction(func(tx *gorm.DB) error { return tx.Exec("UPDATE nodes SET hostname = hostname").Error }); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if reports := stallwatch.Reports(dir); len(reports) != 0 {
		t.Fatalf("reports for statements that returned in time: %v", reports)
	}

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
	if got := len(stallwatch.Reports(dir)); got != 1 {
		t.Errorf("%d reports, want the one stall", got)
	}
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
// both be in the report, each under its own name; a panic inside a
// transaction leaves no wait behind.
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

	func() {
		defer func() { _ = recover() }()
		_ = st.db.Transaction(func(tx *gorm.DB) error { panic("inside a transaction") })
	}()
	time.Sleep(300 * time.Millisecond)
	if reports := stallwatch.Reports(dir); len(reports) != 0 {
		t.Fatalf("a panic inside a transaction left a wait behind: %v", reports)
	}

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
