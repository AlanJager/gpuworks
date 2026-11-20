//go:build linux && dqlite

package cluster

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const zeroRowWriterSQL = "UPDATE cluster_identities SET id = id WHERE 0"

func writerAcquisitionDatabase(t *testing.T, ctx context.Context, dir string) *Database {
	t.Helper()
	db, err := OpenSingleDatabase(ctx, dir, "writer-acquisition.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if db.SQL.Stats().MaxOpenConnections != 4 {
		t.Fatal("fixture changed normal pool4")
	}
	for _, query := range []string{
		"CREATE TABLE cluster_identities (id INTEGER PRIMARY KEY CHECK(id=1), cluster_id TEXT NOT NULL UNIQUE, ca_fingerprint TEXT NOT NULL, protocol INTEGER NOT NULL, created_at TEXT NOT NULL)",
		"INSERT INTO cluster_identities VALUES(1,'fixed-cluster','fixed-authority',1,'2026-01-01T00:00:00Z')",
		"CREATE TABLE grant_counter (id INTEGER PRIMARY KEY, sequence INTEGER NOT NULL)",
		"INSERT INTO grant_counter VALUES(1,0)",
	} {
		if _, err := db.SQL.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestNativeZeroRowWriterAcquiresBeforeRead(t *testing.T) {
	requireDatabaseHelper(t)
	for _, mode := range []string{"commit-release", "rollback-release", "caller-cancel"} {
		t.Run(mode, func(t *testing.T) {
			canceled := mode == "caller-cancel"
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			db := writerAcquisitionDatabase(t, ctx, t.TempDir())
			first, err := db.SQL.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			second, err := db.SQL.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer second.Close()
			held, err := first.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer held.Rollback()
			// No SELECT has established a snapshot. Even with no matching rows this
			// operation must own the native writer for the entire explicit transaction.
			result, err := held.ExecContext(ctx, zeroRowWriterSQL)
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 0 {
				t.Fatalf("zero-row acquisition rows=%d err=%v", rows, err)
			}
			workCtx := ctx
			stopWork := func() {}
			if canceled {
				workCtx, stopWork = context.WithTimeout(ctx, 200*time.Millisecond)
			}
			defer stopWork()
			started, done := make(chan struct{}), make(chan error, 1)
			began := time.Now()
			go func() {
				close(started)
				_, err := second.ExecContext(workCtx, "UPDATE grant_counter SET sequence=sequence+1 WHERE id=1")
				done <- err
			}()
			<-started
			select {
			case err := <-done:
				t.Fatalf("competing native write completed before any owner read or release: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			var before int
			if err := held.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&before); err != nil || before != 0 {
				t.Fatalf("competing write escaped zero-row lock: sequence=%d err=%v", before, err)
			}
			if canceled {
				select {
				case err := <-done:
					// Socket deadline may fire just before the context timer callback.
					// Synchronize the same original caller deadline; do not extend it.
					select {
					case <-workCtx.Done():
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if err == nil || workCtx.Err() == nil {
						t.Fatalf("queued cancellation err=%v ctx=%v", err, workCtx.Err())
					}
				case <-time.After(2 * time.Second):
					t.Fatal("queued caller cancellation ignored")
				}
				if time.Since(began) > 2*time.Second {
					t.Fatal("queued cancellation exceeded original bound")
				}
			}
			// The lock acquisition is not quorum proof: retain a genuine distinct
			// mutation and its confirmed COMMIT before permitting the other writer.
			result, err = held.ExecContext(ctx, "UPDATE grant_counter SET sequence=sequence+1 WHERE id=1")
			if err != nil {
				t.Fatal(err)
			}
			if rows, err := result.RowsAffected(); err != nil || rows != 1 {
				t.Fatalf("distinct counter mutation rows=%d err=%v", rows, err)
			}
			if mode == "rollback-release" {
				if err := held.Rollback(); err != nil {
					t.Fatal(err)
				}
			} else if err := held.Commit(); err != nil {
				t.Fatal(err)
			}
			if !canceled {
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("competitor did not commit after release")
				}
				var sequence int
				want := 2
				if mode == "rollback-release" {
					want = 1
				}
				if err := db.SQL.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&sequence); err != nil || sequence != want {
					t.Fatalf("both confirmed counter writes sequence=%d err=%v", sequence, err)
				}
			} else {
				// A canceled native autocommit has an unknown outcome. Do not replay it;
				// an independent third write proves its queue/connection no longer blocks.
				if _, err := db.SQL.ExecContext(ctx, "UPDATE grant_counter SET sequence=sequence+1 WHERE id=1"); err != nil {
					t.Fatal(err)
				}
				var sequence int
				if err := db.SQL.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&sequence); err != nil || sequence < 2 || sequence > 3 {
					t.Fatalf("post-cancel recovery sequence=%d err=%v", sequence, err)
				}
				t.Logf("canceled autocommit remains unknown; subsequent confirmed counter sequence=%d", sequence)
			}
		})
	}
}

func TestNativeZeroRowWriterAvoidsIdentityRaftPayload(t *testing.T) {
	requireDatabaseHelper(t)
	var sameBytes, zeroBytes int64
	var sameIndex uint64
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-value-primary-key", true: "zero-row"}[zero], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			dir := t.TempDir()
			db := writerAcquisitionDatabase(t, ctx, dir)
			const grants = 16
			acquire := "UPDATE cluster_identities SET id=id WHERE id=1"
			wantRows := int64(1)
			if zero {
				acquire = zeroRowWriterSQL
				wantRows = 0
			}
			for i := 0; i < grants; i++ {
				tx, err := db.SQL.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				err = func() error {
					defer tx.Rollback()
					result, err := tx.ExecContext(ctx, acquire)
					if err != nil {
						return err
					}
					if rows, err := result.RowsAffected(); err != nil || rows != wantRows {
						return fmt.Errorf("acquisition rows=%d want%d err=%v", rows, wantRows, err)
					}
					var sequence int
					if err := tx.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&sequence); err != nil {
						return err
					}
					if sequence != i {
						return fmt.Errorf("initial sequence=%d want%d", sequence, i)
					}
					result, err = tx.ExecContext(ctx, "UPDATE grant_counter SET sequence=sequence+1 WHERE id=1")
					if err != nil {
						return err
					}
					if rows, err := result.RowsAffected(); err != nil || rows != 1 {
						return fmt.Errorf("distinct mutation rows=%d err=%v", rows, err)
					}
					if err := tx.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&sequence); err != nil {
						return err
					}
					if sequence != i+1 {
						return fmt.Errorf("post-mutation sequence=%d want%d", sequence, i+1)
					}
					return tx.Commit()
				}()
				if err != nil {
					t.Fatal(err)
				}
			}
			var sequence int
			if err := db.SQL.QueryRowContext(ctx, "SELECT sequence FROM grant_counter WHERE id=1").Scan(&sequence); err != nil || sequence != grants {
				t.Fatalf("confirmed actual mutations=%d want%d err=%v", sequence, grants, err)
			}
			var id, protocol int
			var clusterID, authority, created string
			if err := db.SQL.QueryRowContext(ctx, "SELECT id,cluster_id,ca_fingerprint,protocol,created_at FROM cluster_identities").Scan(&id, &clusterID, &authority, &protocol, &created); err != nil {
				t.Fatal(err)
			}
			if id != 1 || clusterID != "fixed-cluster" || authority != "fixed-authority" || protocol != 1 || created != "2026-01-01T00:00:00Z" {
				t.Fatal("acquisition changed identity")
			}
			// Stop all owned transport and SQL pools before the native reader can
			// normalize preallocated open logs into finalized, logical-length files.
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			index := lastRaftEntryIndex(t, ctx, dir)
			if index < grants || index >= 1024 {
				t.Fatalf("unexpected actual entry count=%d", index)
			}
			size := writerAcquisitionClosedLogBytes(t, dir)
			if zero {
				zeroBytes = size
				if index != sameIndex {
					t.Fatalf("different actual commit/log counts: old%d zero%d", sameIndex, index)
				}
			} else {
				sameBytes = size
				sameIndex = index
			}
			t.Logf("actual finalized native Raft log: committed_counter=%d raft_index=%d bytes=%d zero_row=%v", grants, index, size, zero)

		})
	}
	if sameBytes <= zeroBytes || zeroBytes <= 0 {
		t.Fatalf("no measured reduction: samevalue=%d zero=%d", sameBytes, zeroBytes)
	}
	t.Logf("same actual16 counter commits and equal Raft index: old=%d zero=%d fewer_Raft_bytes=%d", sameBytes, zeroBytes, sameBytes-zeroBytes)
}

// The stopped native parser validates and normalizes its own log. This helper
// only sums finalized actual files; it does not infer bytes from SQL text or
// interpret Raft framing. Open preallocation and snapshots are never counted.
func writerAcquisitionClosedLogBytes(t *testing.T, dir string) int64 {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, entry := range entries {
		kind, err := offlinePreviewSegmentKind(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		if kind == "snapshot" {
			t.Fatal("native snapshot prevents whole-log byte comparison")
		}
		if kind != "closed" {
			continue
		}
		stat, err := os.Stat(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !stat.Mode().IsRegular() || stat.Size() <= 8 {
			t.Fatal("native closed log is not complete")
		}
		size += stat.Size()
	}
	if size <= 0 {
		t.Fatal("native reader produced no finalized log bytes")
	}
	return size
}
