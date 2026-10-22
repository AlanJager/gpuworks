package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// TestStoreReaderFallsBackToWriterWhenUnset covers the fallback documented on
// reader(): a Store built by hand without going through Open (a handful of
// migration test fixtures do this — see e.g. legacy_migration_test.go) has no
// readDB, and every read path must still work off the single db handle.
func TestStoreReaderFallsBackToWriterWhenUnset(t *testing.T) {
	s := &Store{db: nil}
	// reader() must not panic and must return exactly s.db (nil here), never
	// synthesize a different handle.
	if got := s.reader(); got != s.db {
		t.Fatalf("reader() = %p, want the writer handle %p", got, s.db)
	}
}

// TestOpenSkipsTheReadPoolForAnInMemoryDatabase is a regression test for the
// bug the read/write split first shipped with: an in-memory database is
// SQLite shared-cache, not WAL, and a second connection pool against it can
// collide with the writer's in-flight transaction and return SQLITE_LOCKED
// immediately (this driver has no unlock-notify retry) — which surfaced as
// spurious "no running instance" 503s from every integration test, since
// they all run in-memory (internal/testenv.newEnv). Open must not create a
// second pool for any of the in-memory DSN forms this package accepts, and
// reader() must fall back to the single writer connection.
func TestOpenSkipsTheReadPoolForAnInMemoryDatabase(t *testing.T) {
	for _, dsn := range []string{"", ":memory:", "file:" + t.Name() + "?mode=memory&cache=shared"} {
		s, err := Open(dsn, bus.New())
		if err != nil {
			t.Fatalf("open(%q): %v", dsn, err)
		}
		if s.readDB != nil {
			t.Fatalf("open(%q) created a read pool for an in-memory database", dsn)
		}
		if got := s.reader(); got != s.db {
			t.Fatalf("open(%q): reader() = %p, want the writer handle %p", dsn, got, s.db)
		}
	}
}

// openFileBackedTestStore opens a real on-disk database, the only case Open
// stands up a second connection pool for (see the readDB field doc).
func openFileBackedTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestOpenCreatesASeparateReaderConnectionPoolForAFileBackedDatabase checks
// Open's wiring for the one case it applies to: readDB is set, and it is a
// different *gorm.DB (and therefore a different underlying *sql.DB
// connection pool) from the writer.
func TestOpenCreatesASeparateReaderConnectionPoolForAFileBackedDatabase(t *testing.T) {
	s := openFileBackedTestStore(t)
	if s.readDB == nil {
		t.Fatal("Open did not set up a read connection pool for a file-backed database")
	}
	if s.readDB == s.db {
		t.Fatal("readDB and db must be distinct *gorm.DB instances")
	}
	writerSQLDB, err := s.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	readerSQLDB, err := s.readDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	if writerSQLDB == readerSQLDB {
		t.Fatal("readDB must not share the writer's *sql.DB connection pool")
	}
	if got := s.reader(); got != s.readDB {
		t.Fatalf("reader() = %p, want the read pool %p", got, s.readDB)
	}
}

// TestReadPoolRejectsWrites is the belt-and-braces check on readOnlyDSN's
// mode=ro (belt) and query_only(1) (braces): a write through the read pool
// must fail outright, not silently succeed and let the two pools' views of
// the database drift apart. Both are carried as DSN parameters specifically
// so every connection the pool opens gets them - not just whichever single
// connection a one-off PRAGMA-via-Exec after Open would have landed on - so
// this concurrently acquires all 4 (SetMaxOpenConns(4) in Open) connections
// and confirms a write fails through each one, not just through whichever
// one a single Exec happens to pick.
func TestReadPoolRejectsWrites(t *testing.T) {
	s := openFileBackedTestStore(t)
	if s.readDB == nil {
		t.Fatal("expected a read pool for a file-backed database")
	}
	readSQLDB, err := s.readDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	const n = 4 // matches Open's readSQLDB.SetMaxOpenConns(4)
	var wg sync.WaitGroup
	barrier := make(chan struct{})
	ready := make(chan struct{}, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Begin (rather than a bare Exec) holds one connection exclusively
			// until Rollback, so n concurrent Begins force the pool to open n
			// distinct physical connections instead of reusing one.
			tx, err := readSQLDB.Begin()
			if err != nil {
				t.Errorf("connection %d: begin: %v", i, err)
				return
			}
			defer tx.Rollback()
			ready <- struct{}{}
			<-barrier
			if _, err := tx.Exec(`UPDATE nodes SET hostname = 'nope' WHERE 1 = 0`); err == nil {
				t.Errorf("connection %d: write succeeded through the read pool, want a read-only error", i)
			}
		}(i)
	}
	for i := 0; i < n; i++ {
		select {
		case <-ready:
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d goroutines acquired their own connection in time; the pool may not actually hold %d connections open concurrently", i, n, n)
		}
	}
	close(barrier)
	wg.Wait()

	// The writer connection must be unaffected: it can still write.
	if _, err := s.RegisterNode("write-still-works", HashToken("tok"), "host-a"); err != nil {
		t.Fatalf("writer connection should be unaffected by the read pool's mode: %v", err)
	}
}

// storeReaderFixture seeds one node, one deployment, and one instance (with a
// folded timeline entry) on a file-backed store, so every hot read path this
// test exercises has data to return.
func storeReaderFixture(t *testing.T) (*Store, *Node, *Instance) {
	t.Helper()
	s := openFileBackedTestStore(t)
	node, err := s.RegisterNode("reader-node-"+t.Name(), HashToken("tok-"+t.Name()), "host-a")
	if err != nil {
		t.Fatalf("register node: %v", err)
	}
	dep, err := s.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}
	inst, err := s.CreateInstance(&Instance{UUID: "reader-inst-" + t.Name(), DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := s.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Phase: "running"}); err != nil {
		t.Fatalf("update instance status: %v", err)
	}
	return s, node, inst
}

// TestHotReadPathsUseTheReaderPoolNotTheWriter is the "inject" proof: after
// closing the writer connection outright, every read path this task moved to
// the reader pool (ListNodes, ListDeployments, ListInstances, InstanceByUUID,
// InstanceTimeline, DeploymentTimeline) must still succeed — if any of them
// still touched s.db, they would fail against the closed connection.
func TestHotReadPathsUseTheReaderPoolNotTheWriter(t *testing.T) {
	s, _, inst := storeReaderFixture(t)

	writerSQLDB, err := s.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := writerSQLDB.Close(); err != nil {
		t.Fatalf("close writer connection: %v", err)
	}

	if nodes, err := s.ListNodes(); err != nil || len(nodes) != 1 {
		t.Fatalf("ListNodes() = %+v, %v; want 1 node, no error", nodes, err)
	}
	if deps, err := s.ListDeployments(); err != nil || len(deps) != 1 {
		t.Fatalf("ListDeployments() = %+v, %v; want 1 deployment, no error", deps, err)
	}
	if insts, err := s.ListInstances(); err != nil || len(insts) != 1 {
		t.Fatalf("ListInstances() = %+v, %v; want 1 instance, no error", insts, err)
	}
	got, err := s.InstanceByUUID(inst.UUID)
	if err != nil || got.UUID != inst.UUID {
		t.Fatalf("InstanceByUUID() = %+v, %v; want the seeded instance, no error", got, err)
	}
	if evs, err := s.InstanceTimeline(inst.UUID, 0, 0); err != nil || len(evs) == 0 {
		t.Fatalf("InstanceTimeline() = %+v, %v; want at least one event, no error", evs, err)
	}
	if evs, err := s.DeploymentTimeline(inst.DeploymentID, 0, 0); err != nil || len(evs) == 0 {
		t.Fatalf("DeploymentTimeline() = %+v, %v; want at least one event, no error", evs, err)
	}
}

// TestListNodesDoesNotBlockBehindALongWriteTransaction is the operational,
// WAL-backed proof: the writer's single connection (SetMaxOpenConns(1)) held
// inside a long-running transaction must not block a concurrent list read,
// which the pre-split single-pool Store could not guarantee (every read
// queued behind whatever write transaction was in flight — scale-review.md
// §3).
func TestListNodesDoesNotBlockBehindALongWriteTransaction(t *testing.T) {
	s := openFileBackedTestStore(t)
	if _, err := s.RegisterNode("wal-node", HashToken("wal-token"), "host-a"); err != nil {
		t.Fatalf("register node: %v", err)
	}

	tx := s.db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin long write transaction: %v", tx.Error)
	}
	// Hold the writer's one connection with an in-flight write inside the
	// transaction (not yet committed), the way a real status-update
	// transaction would.
	if err := tx.Model(&Node{}).Where("1 = 1").Update("hostname", "still-writing").Error; err != nil {
		t.Fatalf("write inside long transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	done := make(chan error, 1)
	go func() {
		_, err := s.ListNodes()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListNodes failed while a write transaction was open: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ListNodes blocked behind a long write transaction; it is not using the read connection pool")
	}
}
