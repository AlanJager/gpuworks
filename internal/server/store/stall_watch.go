package store

import (
	"context"
	"database/sql"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// What a database wait is called in a stall report.
const (
	// StallSiteStatement is one statement: waiting for a connection (the
	// single writer's among them), for the disk or, on a replicated store,
	// for the database process.
	StallSiteStatement = "database statement"
	// StallSiteBegin is the start of a transaction: on the writer this is
	// where a write waits for the one before it.
	StallSiteBegin = "database transaction begin"
	// StallSiteCommit is a commit: where a write waits for the log to reach
	// the disk or, on a replicated store, a majority of members.
	StallSiteCommit = "database commit"
	// StallSiteRollback is a rollback.
	StallSiteRollback = "database rollback"
)

// watchDatabase tells the process's stall watch (internal/stallwatch) how
// long each call into db's connection pool has been waiting: every statement,
// and the begin, commit and rollback of every transaction, which GORM runs
// outside its statement callbacks. Each wait ends in a defer, so a panic in
// the driver leaves none behind.
func watchDatabase(db *gorm.DB) {
	pool := &watchedPool{ConnPool: db.ConnPool}
	db.ConnPool = pool
	db.Statement.ConnPool = pool
}

// isDatabasePool says whether pool is the database's own connection pool
// rather than a transaction or a held connection.
func isDatabasePool(pool gorm.ConnPool) bool {
	if watched, ok := pool.(*watchedPool); ok {
		pool = watched.ConnPool
	}
	_, ok := pool.(*sql.DB)
	return ok
}

// watchedPool is a connection pool whose calls the stall watch sees.
type watchedPool struct{ gorm.ConnPool }

func (p *watchedPool) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	defer stallwatch.Begin(StallSiteStatement)()
	return p.ConnPool.PrepareContext(ctx, query)
}

func (p *watchedPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	defer stallwatch.Begin(StallSiteStatement)()
	return p.ConnPool.ExecContext(ctx, query, args...)
}

func (p *watchedPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	defer stallwatch.Begin(StallSiteStatement)()
	return p.ConnPool.QueryContext(ctx, query, args...)
}

func (p *watchedPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	defer stallwatch.Begin(StallSiteStatement)()
	return p.ConnPool.QueryRowContext(ctx, query, args...)
}

// BeginTx starts a transaction the watch sees too. GORM asks the pool it was
// given, which is either database/sql's or one that begins in its own way
// (the replicated writer's admission).
func (p *watchedPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	defer stallwatch.Begin(StallSiteBegin)()
	switch beginner := p.ConnPool.(type) {
	case gorm.TxBeginner:
		tx, err := beginner.BeginTx(ctx, options)
		if err != nil {
			return nil, err
		}
		return &watchedTx{watchedPool: watchedPool{ConnPool: tx}, tx: tx}, nil
	case gorm.ConnPoolBeginner:
		tx, err := beginner.BeginTx(ctx, options)
		if err != nil {
			return nil, err
		}
		committer, ok := tx.(gorm.TxCommitter)
		if !ok {
			return nil, gorm.ErrInvalidTransaction
		}
		return &watchedTx{watchedPool: watchedPool{ConnPool: tx}, tx: committer}, nil
	}
	return nil, gorm.ErrInvalidTransaction
}

// GetDBConn is the database behind the pool, as (*gorm.DB).DB asks for it.
func (p *watchedPool) GetDBConn() (*sql.DB, error) {
	if connector, ok := p.ConnPool.(gorm.GetDBConnector); ok {
		return connector.GetDBConn()
	}
	if db, ok := p.ConnPool.(*sql.DB); ok {
		return db, nil
	}
	return nil, gorm.ErrInvalidDB
}

// watchedTx is a transaction whose statements, commit and rollback the stall
// watch sees. It begins nothing itself: inside it GORM uses savepoints.
type watchedTx struct {
	watchedPool
	tx gorm.TxCommitter
}

func (t *watchedTx) BeginTx(context.Context, *sql.TxOptions) (gorm.ConnPool, error) {
	return nil, gorm.ErrInvalidTransaction
}

func (t *watchedTx) GetDBConn() (*sql.DB, error) { return nil, gorm.ErrInvalidDB }

func (t *watchedTx) Commit() error {
	defer stallwatch.Begin(StallSiteCommit)()
	return t.tx.Commit()
}

func (t *watchedTx) Rollback() error {
	defer stallwatch.Begin(StallSiteRollback)()
	return t.tx.Rollback()
}
