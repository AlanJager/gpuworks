package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// replicatedDialect retains the existing SQLite schema and clause behavior.
// The pinned dqlite SQLite supports RETURNING. Unlike SQLite's default
// initialization, this does not query sqlite_version on a follower through its
// deliberately local-only scheduling connection.
type replicatedDialect struct{ sqlite.Dialector }

func (d replicatedDialect) Initialize(db *gorm.DB) error {
	db.ConnPool = d.Conn
	callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{
		CreateClauses:        []string{"INSERT", "VALUES", "ON CONFLICT", "RETURNING"},
		UpdateClauses:        []string{"UPDATE", "SET", "FROM", "WHERE", "RETURNING"},
		DeleteClauses:        []string{"DELETE", "FROM", "WHERE", "RETURNING"},
		LastInsertIDReversed: true,
	})
	for name, builder := range d.ClauseBuilders() {
		db.ClauseBuilders[name] = builder
	}
	return nil
}

// OpenReplicated binds existing SQL pools without migrations, seeding, or
// process recovery. On success Store owns both pools; on failure the caller
// retains ownership. InitializeSchema must be called explicitly by the
// authorized initializer before serving an uninitialized database. backend is
// the database the pools connect to: the Store classifies their errors by it.
func OpenReplicated(ctx context.Context, normal, scheduling *sql.DB, b *bus.Bus, backend RuntimeBackend) (*Store, error) {
	if normal == nil || scheduling == nil || normal == scheduling {
		return nil, errors.New("replicated storage requires separate normal and local-only scheduling pools")
	}
	if err := normal.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("connect replicated storage: %w", err)
	}
	bind := func(pool gorm.ConnPool) (*gorm.DB, error) {
		return gorm.Open(replicatedDialect{sqlite.Dialector{Conn: pool}}, &gorm.Config{
			DisableAutomaticPing: true,
			Logger:               logger.Default.LogMode(logger.Silent),
			NowFunc:              func() time.Time { return time.Now().UTC() },
		})
	}
	db, err := bind(&replicatedWriterPool{DB: normal})
	if err != nil {
		return nil, err
	}
	local, err := bind(scheduling)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, schedulingDB: local, Bus: b, backend: backend}, nil
}

// InitializeSchema applies migrations and default configuration. It never
// performs global terminal or benchmark recovery merely because a member is
// starting. On a replicated store this operation is confined to the local
// leader connection. Its bounded context does not escape into later requests.
func (s *Store) InitializeSchema(ctx context.Context) error {
	db := s.db
	if s.schedulingDB != nil {
		db = s.schedulingDB
	}
	// Store holds mutexes, so schema setup uses its own value instead of copying s.
	initializing := &Store{db: db.WithContext(ctx), schedulingDB: s.schedulingDB, Bus: s.Bus}
	initializing.readDB = initializing.db
	if err := checkKnownSchema(initializing.db); err != nil {
		return err
	}
	if err := initializing.migrate(); err != nil {
		return err
	}
	return initializing.seedDefaults()
}

// Reject newer databases before migrations or defaults can mutate them. An
// older executable is not a rollback procedure for a newer schema.
func checkKnownSchema(db *gorm.DB) error {
	var exists int
	if err := db.Raw("SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'").Row().Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		var objects int
		if err := db.Raw("SELECT count(*) FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'").Row().Scan(&objects); err != nil {
			return err
		}
		if objects != 0 {
			return errors.New("nonempty database has no schema version; explicit import or recovery is required")
		}
		return nil
	}
	var current int
	if err := db.Raw("SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Row().Scan(&current); err != nil {
		return err
	}
	var supported int
	for _, migration := range migrations {
		if migration.Version > supported {
			supported = migration.Version
		}
	}
	if current > supported {
		return fmt.Errorf("database schema %d is newer than supported version %d; use a compatible server or explicit backup recovery", current, supported)
	}
	return nil
}
