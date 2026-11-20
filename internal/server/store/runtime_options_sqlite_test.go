//go:build !linux || !dqlite

package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Without native dqlite the runtime open is the SQLite store under the
// runtime lease, with the same extension schemas OpenWith applies.
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

// Without the replicated database no read error is a leadership transfer.
func TestReplicatedReadInterruptedIsFalseWithoutReplication(t *testing.T) {
	for _, err := range []error{nil, context.Canceled, os.ErrDeadlineExceeded} {
		if ReplicatedReadInterrupted(err) {
			t.Fatalf("%v classified as an interrupted replicated read", err)
		}
	}
}
