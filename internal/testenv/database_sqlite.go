//go:build !linux || !dqlite

package testenv

// defaultDatabase is the harness database when a test names none: SQLite in
// memory, which this build's server opens directly.
func defaultDatabase(string) string { return ":memory:" }
