package store

import (
	"errors"
	"syscall"

	sqlite "github.com/glebarez/go-sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// IsStorageFull reports an error that says the server's disk has no room
// left: SQLite's SQLITE_FULL ("database or disk is full", in any of its
// extended forms, raised for the database file, its WAL or a temporary file)
// or a file write that failed with ENOSPC. Any of the server's SQLite
// databases qualifies, not only the main one: the driver is the same.
func IsStorageFull(err error) bool {
	if err == nil {
		return false
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_FULL {
		return true
	}
	return errors.Is(err, syscall.ENOSPC)
}
