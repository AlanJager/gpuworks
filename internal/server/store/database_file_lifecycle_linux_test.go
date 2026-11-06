//go:build linux

package store

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func assertDatabaseOpenRefused(t *testing.T, dsn string) {
	t.Helper()
	s, err := Open(dsn, bus.New())
	if s != nil {
		if closeErr := s.Close(); closeErr != nil {
			t.Errorf("close unexpected Store: %v", closeErr)
		}
	}
	if err == nil {
		t.Error("active database permission change was accepted; want an error without opening the file")
	}
}

func TestActiveDatabaseRejectsDifferentPermissionPolicy(t *testing.T) {
	for _, form := range []string{"plain", "file_uri", "relative"} {
		t.Run(form, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "shared database.db")
			openSQLiteLockStore(t, path, securefile.GroupMode)
			dsn := path
			switch form {
			case "file_uri":
				dsn = "file:" + (&url.URL{Path: path}).EscapedPath() + "?cache=shared"
			case "relative":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				dsn, err = filepath.Rel(cwd, path)
				if err != nil {
					t.Fatal(err)
				}
			}
			assertDatabaseOpenRefused(t, dsn)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != securefile.GroupMode {
				t.Errorf("active group policy changed to %#o", info.Mode().Perm())
			}
			assertSQLiteStoreLocks(t, path, "after rejected policy change")
		})
	}
}

func TestActiveDatabaseRejectsPermissionDriftRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.db")
	openSQLiteLockStore(t, path, securefile.OwnerMode)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, securefile.OwnerMode) })
	assertDatabaseOpenRefused(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Errorf("online repair changed drifted mode to %#o", info.Mode().Perm())
	}
	assertSQLiteStoreLocks(t, path, "after rejected online repair")
}

func TestClosedStoreWithBorrowedConnectionStillProtectsDatabase(t *testing.T) {
	for _, pool := range []string{"writer", "reader"} {
		t.Run(pool, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "management.db")
			s := openSQLiteLockStore(t, path, securefile.OwnerMode)
			db := s.db
			if pool == "reader" {
				db = s.readDB
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			conn, err := sqlDB.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			var tables int
			if err := conn.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master").Scan(&tables); err != nil || tables == 0 {
				t.Fatalf("borrowed connection did not read the migrated database: tables=%d error=%v", tables, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			assertSQLiteStoreLocks(t, path, "while closed Store still has a borrowed connection")
			if err := os.Chmod(path, 0644); err != nil {
				t.Fatal(err)
			}
			assertDatabaseOpenRefused(t, path)
			assertSQLiteStoreLocks(t, path, "after rejected repair with a borrowed connection")
			if err := conn.Close(); err != nil {
				t.Fatal(err)
			}
			openSQLiteLockStore(t, path, securefile.OwnerMode)
			assertSQLiteStoreLocks(t, path, "after final connection closed and offline repair")
		})
	}
}

func TestFailedStoreOpenAllowsLaterOfflinePermissionRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "management.db")
	seed := openSQLiteLockStore(t, path, securefile.OwnerMode)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a SQLite database"), securefile.OwnerMode); err != nil {
		t.Fatal(err)
	}
	failed, err := Open(path, bus.New())
	if failed != nil {
		_ = failed.Close()
	}
	if err == nil {
		t.Fatal("corrupt database unexpectedly opened")
	}
	// Preserve the inode: replacing the file would hide a leaked reservation.
	if err := os.WriteFile(path, original, securefile.OwnerMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("failed-open fixture changed inode")
	}
	openSQLiteLockStore(t, path, securefile.OwnerMode)
	assertSQLiteStoreLocks(t, path, "after failed Open cleanup and offline repair")
}

func TestConcurrentStoreOpensPreserveSharedDatabaseLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared database.db")
	seed := openSQLiteLockStore(t, path, securefile.OwnerMode)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	type openResult struct {
		store *Store
		err   error
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan openResult, 2)
	for _, dsn := range []string{path, "file:" + (&url.URL{Path: path}).EscapedPath()} {
		go func(dsn string) {
			ready <- struct{}{}
			<-start
			s, err := Open(dsn, bus.New())
			results <- openResult{store: s, err: err}
		}(dsn)
	}
	for range 2 {
		<-ready
	}
	close(start)
	var stores []*Store
	for range 2 {
		result := <-results
		if result.store != nil {
			stores = append(stores, result.store)
			t.Cleanup(func() {
				if err := result.store.Close(); err != nil {
					t.Errorf("close concurrent Store: %v", err)
				}
			})
		}
		if result.err != nil {
			t.Errorf("concurrent Open: %v", result.err)
		}
	}
	if t.Failed() {
		return
	}
	if len(stores) != 2 {
		t.Fatalf("concurrent Open returned %d Stores, want 2", len(stores))
	}
	assertSQLiteStoreLocks(t, path, "after concurrent Opens")
	if err := stores[0].Close(); err != nil {
		t.Fatal(err)
	}
	assertSQLiteStoreLocks(t, path, "after one concurrent Store closes")
	if err := stores[1].Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	openSQLiteLockStore(t, path, securefile.OwnerMode)
	assertSQLiteStoreLocks(t, path, "after both concurrent Stores close and offline repair")
}
