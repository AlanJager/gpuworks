package store

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
)

func protectDatabase(dsn string, create bool, policies ...securefile.Policy) error {
	policy, err := securefile.Select(policies...)
	if err != nil {
		return err
	}
	path, err := DatabaseFilePath(dsn)
	if err != nil {
		return err
	}
	if path == "" {
		return nil
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("secure database %s: inspect parent: %w", path, err)
	}
	defer directory.Close()
	info, err := directory.Stat()
	if err != nil {
		return fmt.Errorf("secure database %s: inspect parent: %w", path, err)
	}
	if err := policy.CheckDirectory(info); err != nil {
		return fmt.Errorf("secure database directory %s: %w", filepath.Dir(path), err)
	}
	if _, group := policy.GroupID(); group {
		if err := securefile.CheckDirectoryACL(directory); err != nil {
			return fmt.Errorf("secure database directory %s: %w", filepath.Dir(path), err)
		}
	}
	for i, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		err := securefile.Protect(p, policy)
		if errors.Is(err, os.ErrNotExist) {
			if i != 0 || !create {
				continue
			}
			if databaseReadOnly(dsn) {
				return err
			}
			f, createErr := securefile.Create(path, policy)
			if errors.Is(createErr, os.ErrExist) {
				err = securefile.Protect(path, policy)
			} else if createErr != nil {
				err = createErr
			} else {
				err = f.Close()
			}
		}
		if err != nil {
			return fmt.Errorf("secure database %s: %w", p, err)
		}
	}
	return nil
}

// DatabaseFilePath follows the glebarez driver's separation of query options
// from ordinary filenames, then SQLite's file-URI semantics. An empty result
// denotes an anonymous or in-memory database with no persistent path.
func DatabaseFilePath(dsn string) (string, error) {
	if dsn == "" {
		return "", nil
	}
	raw := dsn
	if pos := strings.IndexByte(raw, '?'); pos >= 1 {
		if _, err := url.ParseQuery(raw[pos+1:]); err != nil {
			return "", fmt.Errorf("parse database options: %w", err)
		}
		raw = raw[:pos]
	}
	if strings.IndexByte(raw, 0) >= 0 {
		return "", errors.New("database filename contains NUL")
	}
	if !strings.HasPrefix(dsn, "file:") {
		if raw == ":memory:" {
			return "", nil
		}
		return raw, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse database URI: %w", err)
	}
	if u.User != nil || (u.Host != "" && !strings.EqualFold(u.Host, "localhost")) {
		return "", errors.New("database URI must name a local file")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("parse database URI options: %w", err)
	}
	modes := query["mode"]
	for _, mode := range modes {
		if mode != modes[0] {
			return "", errors.New("conflicting database URI modes")
		}
	}
	path := u.Path
	if u.Opaque != "" {
		path, err = url.PathUnescape(u.Opaque)
		if err != nil {
			return "", fmt.Errorf("decode database URI path: %w", err)
		}
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("database URI filename contains NUL")
	}
	if path == ":memory:" || len(modes) > 0 && modes[0] == "memory" {
		return "", nil
	}
	return path, nil
}

func dbFilePath(dsn string) string { path, _ := DatabaseFilePath(dsn); return path }

func databaseReadOnly(dsn string) bool {
	if !strings.HasPrefix(dsn, "file:") {
		return false
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return false
	}
	values, err := url.ParseQuery(u.RawQuery)
	return err == nil && values.Get("mode") == "ro"
}

// readOnlyDSN preserves SQLite's filename while adding read-pool safeguards.
// A plain filename must be URI-escaped before it becomes a file: URI.
func readOnlyDSN(dsn string) string {
	if !strings.HasPrefix(dsn, "file:") {
		path, query := dsn, ""
		if pos := strings.IndexByte(dsn, '?'); pos >= 1 {
			path, query = dsn[:pos], dsn[pos+1:]
		}
		dsn = "file:" + (&url.URL{Path: path}).EscapedPath()
		if query != "" {
			dsn += "?" + query
		}
	}
	fragment := ""
	if pos := strings.IndexByte(dsn, '#'); pos >= 0 {
		dsn, fragment = dsn[:pos], dsn[pos:]
	}
	const params = "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	if strings.Contains(dsn, "?") {
		return dsn + "&" + params + fragment
	}
	return dsn + "?" + params + fragment
}
