package store

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// replicatedWriterPool is the pool every write of a replicated Store goes
// through, for both of its SQL pools. GORM begins outside database/sql's
// connection acquisition. Transaction views retain the returned *sql.Tx, so
// nested transactions use savepoints without beginning here again.
//
// It is also where a cluster member's writes are refused once the member's
// admission is gone or revoked (writerGuard): the check sits in the pool, not
// in each Store method, so a write added later cannot be without it.
type replicatedWriterPool struct {
	*sql.DB
	// queued says the pool queues its writers and owns the database writer
	// from the first statement (the normal pool). The local scheduling pool
	// begins a plain transaction.
	queued bool
	guard  *writerGuard
}

// writerGuard holds what the pools of one replicated Store need to refuse a
// member that is no longer admitted. It is empty until the Store is bound to
// its member key; a Store that is not a cluster member never is.
type writerGuard struct {
	key atomic.Pointer[string]
	// refused is called, on its own goroutine, when a write was refused
	// because this member is not admitted.
	refused atomic.Pointer[func()]
	// off disables the check. Tests use it as their negative control.
	off atomic.Bool
	// opening is set while the Store's constructor does its own startup work
	// (schema check or migration, default rows, orphaned sessions). That work
	// belongs to the database, not to a request, and must also run on an
	// installation whose admission has yet to be recovered. Nothing else can
	// reach the Store before the constructor returns it.
	opening atomic.Bool
}

type writerGuardExemptKey struct{}

// withoutWriterGuard marks the cluster's own writes: admission, grants,
// revocation, the seed, restore and import. They decide membership or run
// where the writer has no admission yet, so they cannot depend on it.
func withoutWriterGuard(ctx context.Context) context.Context {
	return context.WithValue(ctx, writerGuardExemptKey{}, true)
}

func (g *writerGuard) applies(ctx context.Context) (string, bool) {
	if g == nil || g.off.Load() || g.opening.Load() || ctx.Value(writerGuardExemptKey{}) != nil {
		return "", false
	}
	key := g.key.Load()
	if key == nil || *key == "" {
		return "", false
	}
	return *key, true
}

// writerAdmittedQuery reads the writer's own admission. It runs as the first
// statement of the transaction that writes. That transaction either owns the
// database writer already or takes it at its first write, which fails if
// anything committed after this read; so no revocation can fall between the
// read and the commit, and the answer is never older than the write. The
// member that runs it must be able to commit, so it is the leader's answer.
const writerAdmittedQuery = "SELECT count(*) FROM cluster_admissions WHERE key_fingerprint = ? AND revoked_at IS NULL"

func (g *writerGuard) check(ctx context.Context, tx *sql.Tx) error {
	key, ok := g.applies(ctx)
	if !ok {
		return nil
	}
	var admitted int64
	if err := tx.QueryRowContext(ctx, writerAdmittedQuery, key).Scan(&admitted); err != nil {
		return errors.Join(ErrBusinessWritesClosed, err)
	}
	if admitted != 0 {
		return nil
	}
	if refused := g.refused.Load(); refused != nil {
		go (*refused)()
	}
	return errors.Join(ErrBusinessWritesClosed, ErrClusterAdmissionRejected)
}

func (p *replicatedWriterPool) begin(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	var tx *sql.Tx
	var err error
	if p.queued {
		tx, err = cluster.BeginWriterTransaction(ctx, p.DB, options)
	} else {
		tx, err = p.DB.BeginTx(ctx, options)
	}
	if err != nil {
		return nil, err
	}
	if options != nil && options.ReadOnly {
		return tx, nil
	}
	if err := p.guard.check(ctx, tx); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (p *replicatedWriterPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.begin(ctx, options)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// ExecContext is a statement outside any transaction. On a member it runs in
// a transaction of its own, so that it carries the admission read too.
func (p *replicatedWriterPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if _, ok := p.guard.applies(ctx); !ok {
		return p.DB.ExecContext(ctx, query, args...)
	}
	tx, err := p.begin(ctx, nil)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *replicatedWriterPool) GetDBConn() (*sql.DB, error) { return p.DB, nil }

// OnWriterRefused registers what runs when a write of this member was refused
// because the member is no longer admitted. The member closes its admission
// there instead of waiting for its next renewal to learn the same.
func (s *Store) OnWriterRefused(fn func()) {
	if s == nil || s.writerGuard == nil {
		return
	}
	if fn == nil {
		s.writerGuard.refused.Store(nil)
		return
	}
	s.writerGuard.refused.Store(&fn)
}
