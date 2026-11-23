package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// replicatedOnSQLite is a replicated Store over two SQLite pools on one file:
// the same pool wrappers and guard as on a cluster member, without the
// cluster's database process.
func replicatedOnSQLite(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "guard.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	st, err := OpenReplicated(t.Context(), open(), open(), bus.New(), RuntimeBackend{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("CREATE TABLE cluster_admissions (key_fingerprint TEXT PRIMARY KEY, revoked_at DATETIME)").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("CREATE TABLE things (id INTEGER PRIMARY KEY, n INTEGER)").Error; err != nil {
		t.Fatal(err)
	}
	return st
}

func guardedPool(t *testing.T, db *gorm.DB) *replicatedWriterPool {
	t.Helper()
	watched, ok := db.Statement.ConnPool.(*watchedPool)
	if !ok {
		t.Fatalf("pool is %T, not the watched pool", db.Statement.ConnPool)
	}
	pool, ok := watched.ConnPool.(*replicatedWriterPool)
	if !ok || pool.guard == nil {
		t.Fatalf("writes do not go through the guarded pool: %T", watched.ConnPool)
	}
	return pool
}

// Both pools of a replicated Store are the guarded pool and share one guard,
// so no write of a member can begin anywhere else.
func TestReplicatedStoreWritesOnlyThroughTheGuardedPool(t *testing.T) {
	st := replicatedOnSQLite(t)
	normal, local := guardedPool(t, st.db), guardedPool(t, st.schedulingDB)
	if normal.guard != local.guard || normal.guard != st.writerGuard {
		t.Fatal("the two pools do not share the Store's guard")
	}
	if !normal.queued || local.queued {
		t.Fatalf("queued: normal=%t local=%t", normal.queued, local.queued)
	}
}

// Every way a write can begin on a member carries the admission read: a
// transaction, GORM's own transaction around a create, a statement outside a
// transaction, the Store's exported handle and the scheduling pool. The
// cluster's own writes are exempt, a store without a member key is not
// checked, and the test seam turns the check off.
func TestReplicatedWriterRefusesAMemberThatIsNotAdmitted(t *testing.T) {
	st := replicatedOnSQLite(t)
	key := strings.Repeat("a", 64)
	count := func() int64 {
		var n int64
		if err := st.db.Raw("SELECT count(*) FROM things").Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	writes := []struct {
		name string
		run  func() error
	}{
		{"transaction", func() error {
			return st.db.Transaction(func(tx *gorm.DB) error { return tx.Exec("INSERT INTO things (n) VALUES (1)").Error })
		}},
		{"statement outside a transaction", func() error { return st.db.Exec("INSERT INTO things (n) VALUES (1)").Error }},
		{"exported handle", func() error { return st.DB().Exec("INSERT INTO things (n) VALUES (1)").Error }},
		{"GORM create", func() error { return st.db.Table("things").Create(map[string]any{"n": 1}).Error }},
		{"scheduling pool transaction", func() error {
			return st.schedulingDB.Transaction(func(tx *gorm.DB) error { return tx.Exec("INSERT INTO things (n) VALUES (1)").Error })
		}},
		{"scheduling pool statement", func() error { return st.schedulingDB.Exec("INSERT INTO things (n) VALUES (1)").Error }},
	}
	all := func(want error, label string) {
		t.Helper()
		for _, w := range writes {
			before := count()
			err := w.run()
			if want == nil && (err != nil || count() != before+1) {
				t.Fatalf("%s, %s: err=%v rows %d -> %d", label, w.name, err, before, count())
			}
			if want != nil && (!errors.Is(err, want) || !errors.Is(err, ErrBusinessWritesClosed) || count() != before) {
				t.Fatalf("%s, %s: err=%v rows %d -> %d", label, w.name, err, before, count())
			}
		}
	}
	all(nil, "no member key bound")

	var refused atomic.Int32
	st.OnWriterRefused(func() { refused.Add(1) })
	st.writerGuard.key.Store(&key)
	all(ErrClusterAdmissionRejected, "member has no admission")
	deadline := time.Now().Add(10 * time.Second)
	for refused.Load() != int32(len(writes)) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := refused.Load(); got != int32(len(writes)) {
		t.Fatalf("refusal reported %d times, want %d", got, len(writes))
	}
	// The cluster's own write admits the member; it cannot depend on the read.
	if err := st.clusterTransaction(t.Context(), func(tx *gorm.DB) error {
		return tx.Exec("INSERT INTO cluster_admissions (key_fingerprint) VALUES (?)", key).Error
	}); err != nil {
		t.Fatalf("cluster write of a member without admission: %v", err)
	}
	all(nil, "admitted member")
	if err := st.clusterTransaction(t.Context(), func(tx *gorm.DB) error {
		return tx.Exec("UPDATE cluster_admissions SET revoked_at = ? WHERE key_fingerprint = ?", time.Now().UTC(), key).Error
	}); err != nil {
		t.Fatal(err)
	}
	all(ErrClusterAdmissionRejected, "revoked member")
	// A read-only transaction and a plain read are not writes.
	if err := st.db.Raw("SELECT count(*) FROM things").Scan(new(int64)).Error; err != nil {
		t.Fatalf("read of a revoked member: %v", err)
	}
	if err := st.db.Transaction(func(tx *gorm.DB) error { return tx.Raw("SELECT 1").Scan(new(int)).Error }, &sql.TxOptions{ReadOnly: true}); err != nil {
		t.Fatalf("read-only transaction of a revoked member: %v", err)
	}
	// Negative control: with the check off, the revoked member's writes commit.
	st.writerGuard.off.Store(true)
	all(nil, "check disabled")
	st.writerGuard.off.Store(false)
	all(ErrClusterAdmissionRejected, "check enabled again")
	if _, ok := st.writerGuard.applies(withoutWriterGuard(context.Background())); ok {
		t.Fatal("an exempt context is still checked")
	}
}

// The check is in the pool, so a new Store method cannot write without it.
// What could go around the pool is a different way to reach the database:
// each such construct in this package is counted here, and a new one fails
// until someone has looked at it and raised the count on purpose.
func TestStoreHasNoUnreviewedWayAroundTheGuardedPool(t *testing.T) {
	reviewed := map[string]map[string]int{
		// Exempt transactions: the cluster's own writes only.
		`withoutWriterGuard\(`: {"cluster_admission.go": 1, "replicated_writer.go": 1},
		// A held connection: a consistent read of the schema for a backup, and
		// the read-only discovery of a closed cluster.
		`\.Conn\(ctx\)`: {"backup_sqlite_schema.go": 1, "closed_cluster_discovery_dqlite.go": 1},
		// GORM's held connection: a read with its preloads.
		`\.Connection\(`: {"node_accelerator_report.go": 1},
		// The raw pool: closing, statistics, and SQLite files opened here
		// (never a member's database).
		`\.DB\(\)`:    {"database_files.go": 1, "database_open.go": 3, "migration_snapshot.go": 1, "plugin_trust.go": 1},
		`GetDBConn\(`: {"replicated_writer.go": 1, "stall_watch.go": 3},
		// Calls on a pool object: the stall watch's pass-through, and import
		// inside its own transaction.
		`ConnPool\.(ExecContext|QueryContext|QueryRowContext|PrepareContext)\(`: {"import_sqlite.go": 1, "stall_watch.go": 4},
		// The member key is set in one place.
		`writerGuard\.key\.Store\(`: {"zz_ha_dqlite_only.go": 1},
		`\.off\.Store\(`:            {},
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]map[string]int{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for pattern := range reviewed {
			var n int
			match := regexp.MustCompile(pattern)
			for _, line := range strings.Split(string(raw), "\n") {
				if code, _, _ := strings.Cut(line, "//"); match.MatchString(code) {
					n++
				}
			}
			if n > 0 {
				if found[pattern] == nil {
					found[pattern] = map[string]int{}
				}
				found[pattern][name] = n
			}
		}
	}
	for pattern, files := range reviewed {
		for name, n := range found[pattern] {
			if files[name] != n {
				t.Errorf("%s: %d use(s) of %s, reviewed %d: a write that goes around the guarded pool is not checked for the member's admission", name, n, pattern, files[name])
			}
		}
		for name, n := range files {
			if found[pattern][name] != n {
				t.Errorf("%s: reviewed %d use(s) of %s, found %d: lower the count", name, n, pattern, found[pattern][name])
			}
		}
	}
}
