package store

import (
	"context"
	"database/sql"
	"sync"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/cluster"
)

// replicatedWriterPool is the pool every write of a replicated Store goes
// through, for both of its SQL pools. GORM begins outside database/sql's
// connection acquisition. Transaction views retain the returned transaction,
// so nested transactions use savepoints without beginning here again.
//
// It is also where a cluster member's writes are refused once the member's
// admission is gone or revoked (writerGuard): the check sits in the pool, not
// in each Store method, so a write added later cannot be without it.
type replicatedWriterPool struct {
	*sql.DB
	// queued says the pool queues its writers (the normal pool). The local
	// scheduling pool begins a plain transaction.
	queued bool
	guard  *writerGuard
}

func (p *replicatedWriterPool) begin(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
	if p.queued {
		return cluster.BeginWriterTransaction(ctx, p.DB, options)
	}
	return p.DB.BeginTx(ctx, options)
}

// ownsWriterAtBegin reports a transaction that holds the database writer
// from its first statement.
func (p *replicatedWriterPool) ownsWriterAtBegin() bool {
	return p.queued && cluster.OwnsWriterAtBegin(p.DB)
}

func (p *replicatedWriterPool) BeginTx(ctx context.Context, options *sql.TxOptions) (gorm.ConnPool, error) {
	tx, err := p.begin(ctx, options)
	if err != nil {
		return nil, err
	}
	if options != nil && options.ReadOnly {
		// Not checked, so it must not write.
		return readOnlyTx{tx}, nil
	}
	// Whether the transaction is checked is decided here, from the context
	// it begins with: statements inside it may carry other contexts.
	if _, ok := p.guard.applies(ctx); !ok {
		return tx, nil
	}
	return &guardedTx{Tx: tx, guard: p.guard, ownsWriter: p.ownsWriterAtBegin()}, nil
}

// guardedTx is a member's write transaction. Its first write is preceded, in
// the same transaction, by taking the database writer (where the transaction
// does not own it yet) and reading the member's own admission. A transaction
// that only reads is not checked and takes no writer, as before.
type guardedTx struct {
	*sql.Tx
	guard      *writerGuard
	ownsWriter bool
	mu         sync.Mutex
	checked    bool
	err        error
}

func (t *guardedTx) admit(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.checked {
		t.checked, t.err = true, t.guard.check(ctx, t.Tx, t.ownsWriter)
	}
	return t.err
}

func (t *guardedTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if !savepointStatement(query) {
		if err := t.admit(ctx); err != nil {
			return nil, err
		}
	}
	return t.Tx.ExecContext(ctx, query, args...)
}

func (t *guardedTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !readStatement(query) {
		if err := t.admit(ctx); err != nil {
			return nil, err
		}
	}
	return t.Tx.QueryContext(ctx, query, args...)
}

func (t *guardedTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if !readStatement(query) {
		if err := t.admit(ctx); err != nil {
			ctx = refusedContext(ctx)
		}
	}
	return t.Tx.QueryRowContext(ctx, query, args...)
}

func (t *guardedTx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	if !readStatement(query) {
		if err := t.admit(ctx); err != nil {
			return nil, err
		}
	}
	return t.Tx.PrepareContext(ctx, query)
}

// readOnlyTx is a read-only transaction of the pool. The database does not
// enforce the option, so the statements that write are refused here.
type readOnlyTx struct{ *sql.Tx }

func (t readOnlyTx) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errWriteNeedsTransaction
}

func (t readOnlyTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if !readStatement(query) {
		return nil, errWriteNeedsTransaction
	}
	return t.Tx.QueryContext(ctx, query, args...)
}

func (t readOnlyTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if !readStatement(query) {
		ctx = refusedContext(ctx)
	}
	return t.Tx.QueryRowContext(ctx, query, args...)
}

func (t readOnlyTx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	if !readStatement(query) {
		return nil, errWriteNeedsTransaction
	}
	return t.Tx.PrepareContext(ctx, query)
}

// ExecContext is a statement outside any transaction. On a member it runs in
// a transaction of its own, so that it carries the admission read too.
func (p *replicatedWriterPool) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	// VACUUM cannot run inside a transaction and changes no row.
	if _, ok := p.guard.applies(ctx); !ok || statementVerb(query) == "VACUUM" {
		return p.DB.ExecContext(ctx, query, args...)
	}
	tx, err := p.begin(ctx, nil)
	if err != nil {
		return nil, err
	}
	if err := p.guard.check(ctx, tx, p.ownsWriterAtBegin()); err != nil {
		_ = tx.Rollback()
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

// QueryContext and QueryRowContext are statements outside any transaction. A
// write that returns rows would go to the database unchecked, so on a member
// it is refused: it belongs in a transaction. A read is never refused.
func (p *replicatedWriterPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if _, ok := p.guard.applies(ctx); ok && !readStatement(query) {
		return nil, errWriteNeedsTransaction
	}
	return p.DB.QueryContext(ctx, query, args...)
}

func (p *replicatedWriterPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if _, ok := p.guard.applies(ctx); ok && !readStatement(query) {
		ctx = refusedContext(ctx)
	}
	return p.DB.QueryRowContext(ctx, query, args...)
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
