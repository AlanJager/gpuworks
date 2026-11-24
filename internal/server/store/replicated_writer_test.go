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
	// The constructor's own startup work is not checked.
	st.writerGuard.opening.Store(true)
	all(nil, "store still opening")
	st.writerGuard.opening.Store(false)
	all(ErrClusterAdmissionRejected, "store opened")
	if _, ok := st.writerGuard.applies(withoutWriterGuard(context.Background())); ok {
		t.Fatal("an exempt context is still checked")
	}
}

// The check is in the pool, so a new Store method cannot write without it.
// What could go around the pool is a different way to reach the database.
// Each such construct under internal/server is counted here, and a new one
// fails until someone has looked at it and raised the count on purpose.
// Enterprise modules live in another repository and are not scanned: they
// reach the database through Store.DB() and ExtensionTables, which are this
// pool.
func TestStoreHasNoUnreviewedWayAroundTheGuardedPool(t *testing.T) {
	reviewed := map[string]map[string]int{
		// Exempt transactions: membership decisions and grants, and loading an
		// image or an import into an empty database.
		`withoutWriterGuard\(`: {"store/cluster_admission.go": 1, "store/replicated_writer_guard.go": 1, "store/restore_image.go": 1, "store/import_sqlite.go": 1,
			// a seam other packages' tests use to show a statement cannot carry it
			"store/writer_guard_test_seam_dqlite.go": 1},
		// A held connection: a consistent read of the schema for a backup, and
		// the read-only discovery of a closed cluster.
		`\.Conn\(ctx\)`: {"store/backup_sqlite_schema.go": 1, "store/closed_cluster_discovery_dqlite.go": 1, "cluster/quorum_snapshot.go": 1},
		// GORM's held connection, which is a raw connection of the pool: one
		// helper, used by one read with its preloads.
		`\.Connection\(`:      {"store/stall_watch.go": 1},
		`watchedConnection\(`: {"store/stall_watch.go": 1, "store/node_accelerator_report.go": 1},
		// The raw pool: closing, statistics, and SQLite files opened here
		// (never a member's database).
		`\.DB\(\)\.DB\(\)|[a-z]db\.DB\(\)|\bdb\.DB\(\)|GDB\.DB\(\)`: {"store/database_files.go": 1, "store/database_open.go": 3, "store/migration_snapshot.go": 1, "store/plugin_trust.go": 1},
		`GetDBConn\(`: {"store/replicated_writer.go": 1, "store/stall_watch.go": 3},
		// Calls on a pool object: the stall watch's pass-through, and import
		// inside its own transaction.
		`ConnPool\.(ExecContext|QueryContext|QueryRowContext|PrepareContext)\(`: {"store/import_sqlite.go": 1, "store/stall_watch.go": 4},
		// The member key is set in one place; the check is never turned off.
		`writerGuard\.key\.Store\(`: {"store/zz_ha_dqlite_only.go": 1},
		`\.off\.Store\(`:            {},
		// Startup work is exempt in the one constructor that does any.
		`\.opening\.Store\(`: {"store/runtime_dqlite.go": 2},
		// A write that returns rows outside a transaction is refused by the
		// pool on a member; none exists.
		"Raw\\(\\s*[\"`]\\s*(INSERT|UPDATE|DELETE|REPLACE)": {},
		// Read-only transactions: reads of a held or raw connection.
		`sql\.TxOptions\{ReadOnly: true\}`: {"store/closed_cluster_discovery_dqlite.go": 1, "store/uninstall_inventory.go": 1, "store/backup_sqlite_schema.go": 1, "cluster/quorum_snapshot.go": 1},
		// Every Store on a cluster's database. Those bound to the member's key
		// are checked: the runtime, a joined member, the legacy joined runtime
		// and the join's authorization refresher. The others are offline tools
		// on a database that is out of service (import, restore, address
		// activation) and the upgrade session, which acts for the operator.
		`OpenReplicated\(`: {"store/replicated.go": 1, "store/runtime_dqlite.go": 2, "store/cluster_join_store_dqlite.go": 2, "store/legacy_joined_runtime_dqlite.go": 1,
			"store/import_runtime_dqlite.go": 1, "store/cluster_upgrade_session_dqlite.go": 1, "app/cluster_restore_linux.go": 2, "app/address_activation_linux.go": 2},
	}
	found := map[string]map[string]int{}
	matchers := map[string]*regexp.Regexp{}
	for pattern := range reviewed {
		matchers[pattern] = regexp.MustCompile(pattern)
	}
	err := filepath.WalkDir("..", func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(strings.TrimPrefix(path, "../"))
		for pattern, match := range matchers {
			var n int
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
		return nil
	})
	if err != nil {
		t.Fatal(err)
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

// commitOnOtherPoolWaits runs inside an open scheduling-pool transaction: it
// starts a write on the normal pool and reports whether that write had to
// wait for the transaction. If it committed instead, the transaction did not
// own the writer, and a write after a read in it fails for good. The returned
// channel yields the other write's result once the transaction has ended.
func commitOnOtherPoolWaits(st *Store) (waited bool, result <-chan error) {
	other := make(chan error, 1)
	go func() { other <- st.db.Exec("INSERT INTO things (n) VALUES (7)").Error }()
	select {
	case err := <-other:
		other <- err
		return false, other
	case <-time.After(300 * time.Millisecond):
		return true, other
	}
}

// A decision on the scheduling pool must survive a commit on the other pool.
// The scheduling pool begins without the writer. If the admission read came
// before the writer was taken it would pin a snapshot, and a heartbeat
// committing before the transaction's write would fail that write for good
// ("database is locked (517)", which is what this test shows with the
// writer acquisition removed). The pool takes the writer at the begin, so
// the other commit waits, also for a transaction that reads before it writes
// (recording a notified instance, establishing the drained maintenance
// point; neither is retried).
func TestSchedulingPoolWriteSurvivesCommitsOnTheOtherPool(t *testing.T) {
	for _, readFirst := range []bool{false, true} {
		st := replicatedOnSQLite(t)
		key := strings.Repeat("a", 64)
		if err := st.db.Exec("INSERT INTO cluster_admissions (key_fingerprint) VALUES (?)", key).Error; err != nil {
			t.Fatal(err)
		}
		st.writerGuard.key.Store(&key)
		var waited bool
		var other <-chan error
		err := st.schedulingDB.Transaction(func(tx *gorm.DB) error {
			if readFirst {
				if err := tx.Raw("SELECT count(*) FROM things").Scan(new(int64)).Error; err != nil {
					return err
				}
			}
			waited, other = commitOnOtherPoolWaits(st)
			return tx.Exec("INSERT INTO things (n) VALUES (8)").Error
		})
		if err != nil || !waited {
			t.Fatalf("read first=%t: the other pool waited=%t, scheduling write: %v", readFirst, waited, err)
		}
		if err := <-other; err != nil {
			t.Fatalf("read first=%t: the waiting write on the other pool: %v", readFirst, err)
		}
	}
}

// A statement of the check that fails says nothing about the member: the
// error comes back as it is, not as a refusal, and nothing is reported.
func TestWriterGuardFailureIsNotARefusal(t *testing.T) {
	st := replicatedOnSQLite(t)
	key := strings.Repeat("a", 64)
	if err := st.db.Exec("INSERT INTO cluster_admissions (key_fingerprint) VALUES (?)", key).Error; err != nil {
		t.Fatal(err)
	}
	var refused atomic.Int32
	st.OnWriterRefused(func() { refused.Add(1) })
	st.writerGuard.key.Store(&key)
	write := func(ctx context.Context, db *gorm.DB) error {
		return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return tx.Exec("INSERT INTO things (n) VALUES (1)").Error })
	}
	notRefused := func(name string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, ErrBusinessWritesClosed) || errors.Is(err, ErrClusterAdmissionRejected) {
			t.Fatalf("%s: err=%v, want the failure itself", name, err)
		}
	}
	// The writer is held elsewhere for longer than the caller waits.
	hold, err := st.db.Statement.ConnPool.(*watchedPool).ConnPool.(*replicatedWriterPool).DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hold.Exec("INSERT INTO things (n) VALUES (0)"); err != nil {
		t.Fatal(err)
	}
	busy, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	notRefused("writer busy", write(busy, st.schedulingDB))
	cancel()
	if err := hold.Rollback(); err != nil {
		t.Fatal(err)
	}
	// The caller's context ends before the check.
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	notRefused("context ended", write(ended, st.db))
	// The admission cannot be read at all.
	if err := st.clusterTransaction(t.Context(), func(tx *gorm.DB) error {
		return tx.Exec("ALTER TABLE cluster_admissions RENAME TO moved").Error
	}); err != nil {
		t.Fatal(err)
	}
	notRefused("admission unreadable", write(t.Context(), st.db))
	notRefused("admission unreadable, statement outside a transaction", st.db.Exec("INSERT INTO things (n) VALUES (1)").Error)
	time.Sleep(50 * time.Millisecond)
	if refused.Load() != 0 {
		t.Fatalf("a failed check was reported as a refusal %d times", refused.Load())
	}
}

// What decides whether a statement reads: its verb, not a word in a comment,
// a literal or a subquery. An unknown shape counts as a write.
func TestStatementVerb(t *testing.T) {
	for _, tc := range []struct {
		query string
		verb  string
		read  bool
	}{
		{"SELECT 1", "SELECT", true},
		{"  select * from t where n = 'UPDATE x'", "SELECT", true},
		{"-- note\nSELECT 1", "SELECT", true},
		{"SELECT/**/1", "SELECT", true},
		{"SELECT--x\n1", "SELECT", true},
		{"SELECT 1;", "SELECT", true},
		{"SELECT 1; -- done\n", "SELECT", true},
		{"SELECT ';' AS s", "SELECT", true},
		{"SELECT 1; DELETE FROM t", "SELECT", false},
		{"SAVEPOINT a; INSERT INTO t (n) VALUES (1)", "SAVEPOINT", false},
		{"PRAGMA foreign_keys = OFF", "PRAGMA", false},
		{"PRAGMA foreign_keys=OFF", "PRAGMA", false},
		{"/* UPDATE */ SELECT 1", "SELECT", true},
		{"-- note\nUPDATE t SET n = 1 RETURNING id", "UPDATE", false},
		{"/* SELECT */ DELETE FROM t RETURNING id", "DELETE", false},
		{"(SELECT 1)", "SELECT", true},
		{"VALUES (1)", "VALUES", true},
		{"WITH a AS (SELECT 'UPDATE ' AS s) SELECT s FROM a", "SELECT", true},
		{"WITH RECURSIVE a(n) AS (VALUES (1) UNION ALL SELECT n + 1 FROM a) SELECT n FROM a", "SELECT", true},
		{"WITH a AS (SELECT 1) INSERT INTO t SELECT * FROM a RETURNING id", "INSERT", false},
		{"with a as materialized (select 1), b as (select 2) update t set n = 1 returning id", "UPDATE", false},
		{"INSERT INTO t (n) VALUES (1) RETURNING id", "INSERT", false},
		{"REPLACE INTO t (n) VALUES (1)", "REPLACE", false},
		{"PRAGMA table_info(t)", "PRAGMA", true},
		{"SAVEPOINT sp1", "SAVEPOINT", false},
		{"VACUUM INTO 'x'", "VACUUM", false},
		{"/* unterminated SELECT 1", "", false},
		{"", "", false},
	} {
		if got := statementVerb(tc.query); got != tc.verb || readStatement(tc.query) != tc.read {
			t.Errorf("%q: verb %q read %t, want %q %t", tc.query, got, readStatement(tc.query), tc.verb, tc.read)
		}
	}
	for _, query := range []string{"SAVEPOINT a", "RELEASE SAVEPOINT a", "ROLLBACK TO SAVEPOINT a"} {
		if !savepointStatement(query) {
			t.Errorf("%q is not taken for a savepoint statement", query)
		}
	}
	for _, query := range []string{"UPDATE t SET n = 1", "SAVEPOINT a; INSERT INTO t (n) VALUES (1)", "RELEASE a; DELETE FROM t"} {
		if savepointStatement(query) {
			t.Errorf("%q is taken for a savepoint statement", query)
		}
	}
}

// Statements that would write without the read are refused on a member; a
// read never is, and the same write inside a transaction runs, checked.
func TestMemberWriteOutsideAWriteTransactionIsRefused(t *testing.T) {
	st := replicatedOnSQLite(t)
	key := strings.Repeat("a", 64)
	if err := st.db.Exec("INSERT INTO cluster_admissions (key_fingerprint) VALUES (?)", key).Error; err != nil {
		t.Fatal(err)
	}
	st.writerGuard.key.Store(&key)
	if err := st.db.Raw("INSERT INTO things (n) VALUES (9) RETURNING id").Scan(new(int)).Error; !errors.Is(err, errWriteNeedsTransaction) {
		t.Fatalf("a write returning rows outside a transaction: %v", err)
	}
	if err := st.db.Raw("-- hidden\nINSERT INTO things (n) VALUES (9) RETURNING id").Scan(new(int)).Error; !errors.Is(err, errWriteNeedsTransaction) {
		t.Fatalf("a write behind a comment outside a transaction: %v", err)
	}
	if err := st.db.Raw("UPDATE things SET n = 1 RETURNING id").Row().Scan(new(int)); err == nil {
		t.Fatal("a single-row write outside a transaction ran")
	}
	if err := st.db.Raw("WITH a AS (SELECT 'UPDATE ' AS s) SELECT count(*) FROM a").Scan(new(int)).Error; err != nil {
		t.Fatalf("a read that mentions a write in a literal was refused: %v", err)
	}
	readOnly := &sql.TxOptions{ReadOnly: true}
	if err := st.db.Transaction(func(tx *gorm.DB) error { return tx.Exec("INSERT INTO things (n) VALUES (10)").Error }, readOnly); !errors.Is(err, errWriteNeedsTransaction) {
		t.Fatalf("a write in a read-only transaction: %v", err)
	}
	if err := st.db.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("DELETE FROM things RETURNING id").Scan(new(int)).Error
	}, readOnly); !errors.Is(err, errWriteNeedsTransaction) {
		t.Fatalf("a write returning rows in a read-only transaction: %v", err)
	}
	var n int64
	if err := st.db.Raw("SELECT count(*) FROM things").Scan(&n).Error; err != nil || n != 0 {
		t.Fatalf("a refused statement changed rows: n=%d err=%v", n, err)
	}
	if err := st.db.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("INSERT INTO things (n) VALUES (9) RETURNING id").Scan(new(int)).Error
	}); err != nil {
		t.Fatalf("a write returning rows inside a transaction: %v", err)
	}
	// Revoked: the same write inside a transaction is refused by the check.
	if err := st.clusterTransaction(t.Context(), func(tx *gorm.DB) error {
		return tx.Exec("UPDATE cluster_admissions SET revoked_at = ?", time.Now().UTC()).Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.db.Transaction(func(tx *gorm.DB) error {
		return tx.Raw("INSERT INTO things (n) VALUES (9) RETURNING id").Scan(new(int)).Error
	}); !errors.Is(err, ErrClusterAdmissionRejected) {
		t.Fatalf("a revoked member's write returning rows inside a transaction: %v", err)
	}
}

// Whether a transaction is checked is fixed when it begins. Neither pool of
// this stand-in owns the writer at its begin, so both are checked there and a
// statement's context comes too late to matter; the case where it could
// matter (a pool that is checked at its first write) only exists on the real
// database and is tested there (TestNativeExemptStatementInside...).
func TestExemptStatementInsideACheckedTransactionIsStillChecked(t *testing.T) {
	st := replicatedOnSQLite(t)
	key := strings.Repeat("a", 64)
	st.writerGuard.key.Store(&key) // no admission row: not admitted
	count := func() (n int64) {
		if err := st.db.Raw("SELECT count(*) FROM things").Scan(&n).Error; err != nil {
			t.Fatal(err)
		}
		return n
	}
	for name, db := range map[string]*gorm.DB{"normal pool": st.db, "scheduling pool": st.schedulingDB} {
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.WithContext(withoutWriterGuard(t.Context())).Exec("INSERT INTO things (n) VALUES (1)").Error; err != nil {
				return err
			}
			return tx.Exec("INSERT INTO things (n) VALUES (2)").Error
		})
		if !errors.Is(err, ErrClusterAdmissionRejected) || count() != 0 {
			t.Fatalf("%s: err=%v rows=%d, want a refusal and no row", name, err, count())
		}
	}
	// The exemption does work where it belongs: on the transaction's begin.
	if err := st.db.WithContext(withoutWriterGuard(t.Context())).Transaction(func(tx *gorm.DB) error {
		return tx.Exec("INSERT INTO things (n) VALUES (3)").Error
	}); err != nil || count() != 1 {
		t.Fatalf("exempt transaction: err=%v rows=%d", err, count())
	}
}
