//go:build linux && dqlite

package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"

	"gorm.io/gorm"
)

// The pool is installed before the App starts. Only an armed manifest-pointer
// write loses its acknowledgement; every other transaction keeps real driver
// behavior, including quorum and receiver-index writes.
type materialManifestCommitFault struct {
	*sql.DB
	armed     atomic.Bool
	committed chan struct{}
	release   chan struct{}
}

func (pool *materialManifestCommitFault) GetDBConn() (*sql.DB, error) { return pool.DB, nil }

func (pool *materialManifestCommitFault) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := pool.DB.BeginTx(ctx, options)
	if err != nil {
		return nil, err
	}
	return &materialManifestCommitFaultTx{Tx: tx, pool: pool, ctx: ctx}, nil
}

type materialManifestCommitFaultTx struct {
	*sql.Tx
	pool     *materialManifestCommitFault
	ctx      context.Context
	manifest bool
}

func (tx *materialManifestCommitFaultTx) track(query string) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "INSERT ") && strings.Contains(query, "material_manifest_records") {
		tx.manifest = true
	}
}

func (tx *materialManifestCommitFaultTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx.track(query)
	return tx.Tx.ExecContext(ctx, query, args...)
}

func (tx *materialManifestCommitFaultTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	tx.track(query)
	return tx.Tx.QueryContext(ctx, query, args...)
}

func (tx *materialManifestCommitFaultTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.manifest && tx.pool.armed.CompareAndSwap(true, false) {
		close(tx.pool.committed)
		select {
		case <-tx.pool.release:
			return errors.New("material manifest commit acknowledgement lost")
		case <-tx.ctx.Done():
			return tx.ctx.Err()
		}
	}
	return nil
}
