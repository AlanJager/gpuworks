//go:build linux && dqlite

package testenv

import "path/filepath"

// defaultDatabase is the harness database when a test names none. The
// clustered server owns a database directory and refuses an in-memory DSN,
// so the harness places it in the test's private data directory.
func defaultDatabase(dir string) string { return filepath.Join(dir, "gpuworks.db") }
