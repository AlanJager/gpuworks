package store

import (
	"context"
	"database/sql/driver"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Without a backend the runtime open is the SQLite store under the runtime
// lease, with the same extension schemas OpenWith applies.
func TestOpenRuntimeWithAppliesExtensionSchemas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	st, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{Extensions: []MigrationSet{quotaSet()}})
	if err != nil {
		t.Fatal(err)
	}
	if got := markers(t, st, "quota"); len(got) != 2 {
		t.Fatalf("recorded steps = %+v, want 2", got)
	}
	if _, err := st.ExtensionTables("quota"); err != nil {
		t.Fatalf("extension tables: %v", err)
	}
	// The lease is held until Close.
	if _, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{}); err == nil || !strings.Contains(err.Error(), "lock runtime database") {
		t.Fatalf("second owner = %v, want a runtime lock refusal", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{Extensions: []MigrationSet{quotaSet()}})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if got := markers(t, again, "quota"); len(got) != 2 {
		t.Fatalf("steps after reopen = %+v", got)
	}
}

func TestOpenRuntimeWithRefusesMarkersAndReplicatedFiles(t *testing.T) {
	for _, tc := range []struct{ name, suffix, want string }{
		{"join marker", ".join", "cluster join is incomplete"},
		{"maintenance marker", ".maintenance", "database maintenance is incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gpuworks.db")
			if err := os.WriteFile(path+tc.suffix, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("open = %v, want %q", err, tc.want)
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Fatalf("a refused open created the database: %v", statErr)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	if err := os.Mkdir(path+".dqlite", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRuntimeWith(context.Background(), path, bus.New(), OpenOptions{}); err == nil || !strings.Contains(err.Error(), "replicated database exists") {
		t.Fatalf("open beside native files = %v", err)
	}
	if _, err := OpenExpandedRuntimeWith(context.Background(), path, bus.New(), OpenOptions{}); err == nil || !strings.Contains(err.Error(), "requires native Linux dqlite") {
		t.Fatalf("expanded open without dqlite = %v", err)
	}
}

// On SQLite no error is a leadership change, whichever way the Store was
// opened, and the Store names the backend it was opened on.
func TestSQLiteBackendClassifiesNoErrorAsReplicated(t *testing.T) {
	st, err := OpenRuntimeWith(context.Background(), filepath.Join(t.TempDir(), "gpuworks.db"), bus.New(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var none *Store
	for _, backend := range []RuntimeBackend{{}, st.RuntimeBackend(), none.RuntimeBackend()} {
		if backend != (RuntimeBackend{}) {
			t.Fatalf("backend = %+v, want the zero SQLite backend", backend)
		}
		for _, err := range []error{nil, context.Canceled, os.ErrDeadlineExceeded, driver.ErrBadConn} {
			if backend.ReplicatedReadInterrupted(err) || backend.ReplicatedLeaderUnavailable(err) {
				t.Fatalf("%v classified as a replicated database error", err)
			}
		}
	}
}

// The opens only a cluster has are refused on SQLite, and none of them
// leaves a database or keeps the runtime lease.
func TestSQLiteBackendRefusesClusterOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	ctx := context.Background()
	for name, open := range map[string]func() (*Store, error){
		"existing":        func() (*Store, error) { return OpenExistingRuntime(ctx, path, bus.New(), RuntimeBackend{}) },
		"expanded":        func() (*Store, error) { return OpenExpandedRuntime(ctx, path, bus.New(), RuntimeBackend{}) },
		"legacy expanded": func() (*Store, error) { return OpenLegacyExpandedRuntime(ctx, path, bus.New(), RuntimeBackend{}) },
	} {
		if st, err := open(); err == nil || !strings.Contains(err.Error(), "native Linux dqlite") {
			_ = st.Close()
			t.Fatalf("%s open = %v, want a refusal", name, err)
		}
	}
	if _, err := OpenRuntimeForMigration(ctx, path, bus.New(), RuntimeBackend{}); err == nil || !strings.Contains(err.Error(), "existing regular SQLite database") {
		t.Fatalf("migration of a missing database = %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a refused open created the database: %v", err)
	}
	st, err := OpenRuntime(ctx, path, bus.New(), RuntimeBackend{})
	if err != nil {
		t.Fatalf("open after the refusals: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenRuntimeForMigration(ctx, path, bus.New(), RuntimeBackend{})
	if err != nil {
		t.Fatalf("offline migration of the SQLite database: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
}
