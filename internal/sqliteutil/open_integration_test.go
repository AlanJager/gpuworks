package sqliteutil_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
)

// Exercise the public initialization boundaries: database/sql defers actually
// opening a connection until a pragma or schema query is executed.
func TestDatabaseOpenReportsInaccessiblePath(t *testing.T) {
	openers := []struct {
		name string
		open func(string) (io.Closer, error)
	}{
		{"management", func(path string) (io.Closer, error) { return store.Open(path, bus.New()) }},
		{"logs", func(path string) (io.Closer, error) { return logstore.OpenSQLite(path) }},
		{"requests", func(path string) (io.Closer, error) { return uxstore.Open(path, uxstore.Options{}) }},
		{"metrics", func(path string) (io.Closer, error) { return tsdb.OpenSQLite(path) }},
		{"statistics", func(path string) (io.Closer, error) { return statsdb.Open(path) }},
	}
	for _, opener := range openers {
		t.Run(opener.name, func(t *testing.T) {
			for _, scenario := range []string{"missing parent", "directory as database"} {
				t.Run(scenario, func(t *testing.T) {
					path := filepath.Join(t.TempDir(), "missing", "database.db")
					if scenario == "directory as database" {
						if err := os.MkdirAll(path, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					db, err := opener.open(path)
					if err == nil {
						_ = db.Close()
						t.Fatal("opening an inaccessible database unexpectedly succeeded")
					}
					var coded interface{ Code() int }
					if !errors.As(err, &coded) || coded.Code()&0xff != 14 {
						t.Fatalf("original SQLITE_CANTOPEN error is not preserved: %v", err)
					}
					if !strings.Contains(err.Error(), path) {
						t.Errorf("database error does not identify path %q: %v", path, err)
					}
					if strings.Contains(strings.ToLower(err.Error()), "out of memory") {
						t.Errorf("database path error misleadingly reports memory exhaustion: %v", err)
					}
				})
			}
		})
	}
}
