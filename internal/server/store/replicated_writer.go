package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

// withoutWriterGuard marks the writes that decide membership or run where the
// writer has no admission yet: claiming an invitation, the seed, revocation,
// membership and transport grants (their own statements require the named
// keys to be admitted, and a member asks about itself through them), and an
// image or import loaded into an empty database. Everything else a member
// writes, including what it records about material while serving, is checked.
func withoutWriterGuard(ctx context.Context) context.Context {
	if exempt, set := ctx.Value(writerGuardExemptKey{}).(bool); set && !exempt {
		return ctx // withWriterGuard was asked for further out; it wins.
	}
	return context.WithValue(ctx, writerGuardExemptKey{}, true)
}

// withWriterGuard keeps the check on a write that runs through a helper
// which would otherwise exempt it.
func withWriterGuard(ctx context.Context) context.Context {
	return context.WithValue(ctx, writerGuardExemptKey{}, false)
}

func (g *writerGuard) applies(ctx context.Context) (string, bool) {
	if exempt, _ := ctx.Value(writerGuardExemptKey{}).(bool); g == nil || g.off.Load() || g.opening.Load() || exempt {
		return "", false
	}
	key := g.key.Load()
	if key == nil || *key == "" {
		return "", false
	}
	return *key, true
}

// writerAdmittedQuery reads the writer's own admission. It runs in the
// transaction that writes, once that transaction owns the database writer, so
// no revocation can fall between the read and the commit and the answer is
// never older than the write. The member that runs it must be able to commit,
// so it is the leader's answer.
const writerAdmittedQuery = "SELECT count(*) FROM cluster_admissions WHERE key_fingerprint = ? AND revoked_at IS NULL"

// writerAcquireStatement takes the database writer without changing a row. A
// transaction that reads first holds a snapshot, and its first write then
// fails for good, with no busy timeout to wait out, if another connection
// committed in between. The read above must therefore come after the writer
// is owned: the queued pool begins owning it, the other pool takes it here.
const writerAcquireStatement = "UPDATE cluster_admissions SET key_fingerprint = key_fingerprint WHERE 0"

func (g *writerGuard) check(ctx context.Context, tx *sql.Tx, ownsWriter bool) error {
	key, ok := g.applies(ctx)
	if !ok {
		return nil
	}
	if !ownsWriter {
		if _, err := tx.ExecContext(ctx, writerAcquireStatement); err != nil {
			return errors.Join(ErrBusinessWritesClosed, err)
		}
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
	if err := p.guard.check(ctx, tx, p.queued && cluster.OwnsWriterAtBegin(p.DB)); err != nil {
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
	if options != nil && options.ReadOnly {
		// Not checked, so it must not write.
		return readOnlyTx{tx}, nil
	}
	return tx, nil
}

// errWriteNeedsTransaction refuses a write that would reach the database
// without the admission read: one that returns rows outside a transaction, or
// any write inside a read-only transaction.
var errWriteNeedsTransaction = errors.New("a write that returns rows runs inside a write transaction; a read-only transaction does not write")

// readOnlyTx is a read-only transaction of the pool. The database does not
// enforce the option, so the statements that write are refused here.
type readOnlyTx struct{ *sql.Tx }

func (t readOnlyTx) ExecContext(context.Context, string, ...any) (sql.Result, error) {
	return nil, errWriteNeedsTransaction
}

func (t readOnlyTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if writesRows(query) {
		return nil, errWriteNeedsTransaction
	}
	return t.Tx.QueryContext(ctx, query, args...)
}

func (t readOnlyTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if writesRows(query) {
		ctx = refusedContext(ctx)
	}
	return t.Tx.QueryRowContext(ctx, query, args...)
}

// writesRows reports a statement that changes rows, as a query can when it
// ends in RETURNING.
func writesRows(query string) bool {
	head := strings.ToUpper(strings.TrimLeft(query, " \t\r\n("))
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "REPLACE"} {
		if strings.HasPrefix(head, verb) {
			return true
		}
	}
	return strings.HasPrefix(head, "WITH") && (strings.Contains(head, "INSERT ") || strings.Contains(head, "UPDATE ") || strings.Contains(head, "DELETE "))
}

// refusedContext is an ended context: database/sql returns a row carrying its
// error without sending the statement. A single-row query has no other way
// to be refused.
func refusedContext(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancelCause(ctx)
	cancel(errWriteNeedsTransaction)
	return ctx
}

// QueryContext and QueryRowContext are statements outside any transaction. A
// write that returns rows would go to the database unchecked, so on a member
// it is refused: it belongs in a transaction.
func (p *replicatedWriterPool) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if _, ok := p.guard.applies(ctx); ok && writesRows(query) {
		return nil, errWriteNeedsTransaction
	}
	return p.DB.QueryContext(ctx, query, args...)
}

func (p *replicatedWriterPool) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if _, ok := p.guard.applies(ctx); ok && writesRows(query) {
		ctx = refusedContext(ctx)
	}
	return p.DB.QueryRowContext(ctx, query, args...)
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
