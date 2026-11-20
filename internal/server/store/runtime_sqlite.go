//go:build !linux || !dqlite

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// openRuntimeDatabase retains native SQLite on macOS and in the explicit pure-Go test
// build. Production Linux dqlite builds use runtime_dqlite.go.
func openRuntimeDatabase(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	if existing {
		return nil, errors.New("offline cluster metadata operations require native Linux dqlite")
	}
	return openRuntimeDatabaseWith(ctx, path, b, OpenOptions{})
}

// openRuntimeDatabaseWith is the SQLite fallback with OpenWith's options.
func openRuntimeDatabaseWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.HasPrefix(path, "file:") {
		return nil, errors.New("runtime database requires a filesystem path, not a SQLite URI")
	}
	if path != "" && path != ":memory:" {
		if _, err := os.Stat(path + ".dqlite"); err == nil {
			return nil, fmt.Errorf("replicated database exists at %q; use a dqlite-enabled Linux server instead of creating a SQLite fallback", path+".dqlite")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return OpenWith(path, b, o)
}

func openRuntimeMigrationDatabase(ctx context.Context, path string, b *bus.Bus, existing bool) (_ *Store, resultErr error) {
	if !existing || path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("offline migration requires an existing filesystem database")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + ".dqlite"); err == nil {
		return nil, errors.New("native replicated database requires the dqlite-enabled migration command")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("offline migration requires an existing regular SQLite database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("offline migration requires a regular SQLite database")
	}
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return nil, err
	}
	st := &Store{db: db, Bus: b}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, st.Close())
		}
	}()
	if err := st.migrateSchemaOffline(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

func openEstablishedClusterRuntimeDatabase(context.Context, string, *bus.Bus, bool) (*Store, error) {
	return nil, errors.New("established cluster runtime requires native Linux dqlite")
}

func openLegacyExpandedRuntimeDatabase(context.Context, string, *bus.Bus, bool) (*Store, error) {
	return nil, errors.New("legacy expanded cluster runtime requires native Linux dqlite")
}
