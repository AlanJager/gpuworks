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

// sqliteRuntimeDatabase is the SQLite backend: one file beside the server,
// no members. The opens that only a cluster has are refused.
type sqliteRuntimeDatabase struct{}

func (d sqliteRuntimeDatabase) open(ctx context.Context, path string, b *bus.Bus, existing bool) (*Store, error) {
	if existing {
		return nil, errors.New("offline cluster metadata operations require native Linux dqlite")
	}
	return d.openWith(ctx, path, b, OpenOptions{})
}

// openWith refuses to create a SQLite file beside a replicated database.
func (sqliteRuntimeDatabase) openWith(ctx context.Context, path string, b *bus.Bus, o OpenOptions) (*Store, error) {
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

func (sqliteRuntimeDatabase) openForMigration(ctx context.Context, path string, b *bus.Bus, existing bool) (_ *Store, resultErr error) {
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
	watchDatabase(db)
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

func (sqliteRuntimeDatabase) openEstablishedCluster(context.Context, string, *bus.Bus, bool) (*Store, error) {
	return nil, errors.New("established cluster runtime requires native Linux dqlite")
}

func (sqliteRuntimeDatabase) openLegacyExpanded(context.Context, string, *bus.Bus, bool) (*Store, error) {
	return nil, errors.New("legacy expanded cluster runtime requires native Linux dqlite")
}

// leaderUnavailable is always false: a local SQLite connection has no leader
// to elect.
func (sqliteRuntimeDatabase) leaderUnavailable(error) bool { return false }

// readInterrupted is always false: a local SQLite read has no leadership
// transfer to retry across.
func (sqliteRuntimeDatabase) readInterrupted(error) bool { return false }
