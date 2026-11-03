// Package sqliteutil provides shared SQLite diagnostics without owning connections.
package sqliteutil

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

type openError struct {
	message string
	cause   error
}

func (e *openError) Error() string { return e.message }
func (e *openError) Unwrap() error { return e.cause }

// WrapOpenError explains SQLITE_CANTOPEN from any stage of opening a database,
// including database/sql's lazy first connection. Other errors are unchanged.
// go-sqlite can append "out of memory" while reporting CANTOPEN because it asks
// sqlite3_errmsg about an uninitialised connection handle. The numeric code is
// authoritative: real SQLITE_NOMEM must retain its original message and cause.
func WrapOpenError(dsn string, err error) error {
	var explained *openError
	if err == nil || errors.As(err, &explained) {
		return err
	}
	var coded interface {
		error
		Code() int
	}
	if !errors.As(err, &coded) || coded.Code()&255 != 14 {
		return err
	}
	message := strings.Replace(err.Error(), coded.Error(),
		fmt.Sprintf("cannot open database file (SQLITE_CANTOPEN, code %d)", coded.Code()), 1)
	path, readOnly, memory := databasePath(dsn)
	if memory {
		return &openError{message: "in-memory SQLite database: " + message, cause: err}
	}
	return &openError{
		message: fmt.Sprintf("SQLite database %q: %s; %s", path, message, pathDiagnostic(path, readOnly)),
		cause:   err,
	}
}

// databasePath mirrors the driver's filename/query split. Only file: URIs
// percent-decode filenames or honor SQLite's mode parameter. Query values are
// never included in the displayed path.
func databasePath(dsn string) (path string, readOnly, memory bool) {
	path = dsn
	var query string
	if pos := strings.IndexByte(dsn, '?'); pos > 0 {
		path, query = dsn[:pos], dsn[pos+1:]
	}
	if strings.HasPrefix(dsn, "file:") {
		if u, err := url.Parse(dsn); err == nil {
			path = u.Path
			if u.Opaque != "" {
				path = u.Opaque
				if decoded, err := url.PathUnescape(path); err == nil {
					path = decoded
				}
			}
		} else {
			path = strings.TrimPrefix(path, "file:")
		}
		params, _ := url.ParseQuery(query)
		readOnly = params.Get("mode") == "ro"
		memory = params.Get("mode") == "memory"
	}
	return path, readOnly, memory || path == "" || path == ":memory:"
}

// Diagnostics only inspect metadata after an open failure. They never create,
// chmod or open a file (in particular a FIFO/device), and make no claim about
// ACLs, mount options or access rights that Stat cannot establish.
func pathDiagnostic(path string, readOnly bool) string {
	info, err := os.Stat(path)
	if err == nil {
		if info.IsDir() {
			return "database path is a directory; choose a database file"
		}
		if !info.Mode().IsRegular() {
			return "database path is not a regular file; choose a database file"
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("cannot inspect database path: %v; check ancestor directory traversal permissions", err)
	} else {
		parent := filepath.Dir(path)
		parentInfo, parentErr := os.Stat(parent)
		switch {
		case errors.Is(parentErr, os.ErrNotExist):
			return fmt.Sprintf("parent directory does not exist: %q", parent)
		case parentErr != nil:
			return fmt.Sprintf("cannot inspect parent directory: %v; check ancestor directory traversal permissions", parentErr)
		case !parentInfo.IsDir():
			return fmt.Sprintf("parent path is not a directory: %q", parent)
		case readOnly:
			return "database file does not exist; a read-only connection requires an existing database"
		default:
			return fmt.Sprintf("database file does not exist; check service account write permission in parent directory %q (mode %s) and ancestor directory traversal permissions", parent, parentInfo.Mode())
		}
	}
	if readOnly {
		return "check service account read permission on the database and ancestor directory traversal permissions"
	}
	return "check service account read/write permissions on the database, parent directory write permission for SQLite WAL/SHM files, ancestor directory traversal permissions, and whether the filesystem is read-only"
}
