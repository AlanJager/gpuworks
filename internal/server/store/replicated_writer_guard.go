package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math/bits"
	"strings"
	"sync/atomic"
)

// writerGuard holds what the pools of one replicated Store need to refuse a
// member that is no longer admitted. It is empty until the Store is bound to
// its member key; a Store that is not a cluster member never is.
type writerGuard struct {
	key atomic.Pointer[string]
	// refused is called, on its own goroutine, when a write was refused
	// because this member is not admitted.
	refused atomic.Pointer[func()]
	// refusals counts the writes refused that way, and log is where they are
	// reported. Neither is touched by a write that is admitted.
	refusals atomic.Uint64
	log      atomic.Pointer[slog.Logger]
	// opening is set while the Store's constructor does its own startup work
	// (schema check or migration, default rows, orphaned sessions). That work
	// belongs to the database, not to a request, and must also run on an
	// installation whose admission has yet to be recovered. Nothing else can
	// reach the Store before the constructor returns it.
	opening atomic.Bool
}

type writerGuardExemptKey struct{}

// withoutWriterGuard exempts a transaction from the check. It is for the
// writes that decide membership (membershipTransaction) and for an image or
// import loaded into an empty database, where the writer has no admission
// yet. Everything else a member writes, including what it records about
// material while serving, is checked.
func withoutWriterGuard(ctx context.Context) context.Context {
	return context.WithValue(ctx, writerGuardExemptKey{}, true)
}

func (g *writerGuard) applies(ctx context.Context) (string, bool) {
	if exempt, _ := ctx.Value(writerGuardExemptKey{}).(bool); g == nil || g.opening.Load() || exempt {
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

// check answers for one transaction of the member with this key. Whether a
// transaction is checked at all was decided when it began (applies); ctx here
// only carries the statements. Only the answer "not admitted" is a refusal.
// A statement of the check that fails (the writer busy, no leader, a dropped
// connection, an expired deadline) is returned as it is: it says nothing
// about this member, and callers classify it as they classify the same
// failure of the write itself.
func (g *writerGuard) check(ctx context.Context, tx *sql.Tx, key string, ownsWriter bool) error {
	if !ownsWriter {
		if _, err := tx.ExecContext(ctx, writerAcquireStatement); err != nil {
			return err
		}
	}
	var admitted int64
	if err := tx.QueryRowContext(ctx, writerAdmittedQuery, key).Scan(&admitted); err != nil {
		return err
	}
	if admitted != 0 {
		return nil
	}
	g.reportRefusal()
	if refused := g.refused.Load(); refused != nil {
		go (*refused)()
	}
	return errors.Join(ErrBusinessWritesClosed, ErrClusterAdmissionRejected)
}

// reportRefusal counts a refused write and logs the first, second, fourth,
// eighth and so on, so a member that keeps writing after it was removed
// leaves a bounded number of lines.
func (g *writerGuard) reportRefusal() {
	n := g.refusals.Add(1)
	if bits.OnesCount64(n) != 1 {
		return
	}
	log := g.log.Load()
	if log == nil {
		log = slog.Default()
	}
	log.Warn("refused a write of this member: the cluster no longer admits it", "refused_writes", n)
}

// errWriteNeedsTransaction refuses a write that would reach the database
// without the admission read: one that returns rows outside a transaction, or
// any write inside a read-only transaction.
var errWriteNeedsTransaction = errors.New("a write that returns rows runs inside a write transaction; a read-only transaction does not write")

// refusedContext is an ended context: database/sql returns a row carrying its
// error without sending the statement. A single-row query has no other way
// to be refused.
func refusedContext(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancelCause(ctx)
	cancel(errWriteNeedsTransaction)
	return ctx
}

// statementVerb is the keyword that decides what a statement does: its first
// word, or for a statement that begins WITH, the first keyword after the
// common table expressions. Comments, quoted text and anything in
// parentheses are skipped, so neither a leading comment nor a word inside a
// literal or a subquery decides it. It is empty for a statement this cannot
// read.
func statementVerb(query string) string {
	var word strings.Builder
	depth, first := 0, ""
	flush := func() string {
		w := strings.ToUpper(word.String())
		word.Reset()
		return w
	}
	decide := func(w string) (string, bool) {
		if w == "" {
			return "", false
		}
		if first == "" {
			first = w
			if w != "WITH" {
				return w, true
			}
			return "", false
		}
		switch w {
		case "SELECT", "VALUES", "INSERT", "UPDATE", "DELETE", "REPLACE":
			return w, true
		}
		return "", false
	}
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			for i < len(query) && query[i] != '\n' {
				i++
			}
			// A comment separates words, as white space does.
			if verb, ok := decide(flush()); ok && depth == 0 {
				return verb
			}
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return ""
			}
			i += end + 3
			if verb, ok := decide(flush()); ok && depth == 0 {
				return verb
			}
		case c == '\'' || c == '"' || c == '`':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				return ""
			}
			i += end + 1
		case c == '[':
			end := strings.IndexByte(query[i+1:], ']')
			if end < 0 {
				return ""
			}
			i += end + 1
		case c == '(':
			if depth == 0 {
				if verb, ok := decide(flush()); ok {
					return verb
				}
				if first == "" {
					continue // a parenthesis around the whole statement
				}
			}
			depth++
		case c == ')':
			if depth > 0 {
				depth--
			}
		case depth == 0 && (c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'):
			word.WriteByte(c)
		case depth == 0:
			if verb, ok := decide(flush()); ok {
				return verb
			}
		}
	}
	verb, _ := decide(flush())
	return verb
}

// singleStatement reports text that holds one statement: nothing but white
// space and comments follows its first semicolon. Quoted text is skipped. A
// second statement could be anything, so text that has one is a write.
func singleStatement(query string) bool {
	for i := 0; i < len(query); i++ {
		switch c := query[i]; {
		case c == '\'' || c == '"' || c == '`':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				return false
			}
			i += end + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return false
			}
			i += end + 3
		case c == ';':
			return statementVerb(query[i+1:]) == "" && strings.TrimSpace(stripComments(query[i+1:])) == ""
		}
	}
	return true
}

// stripComments removes comments outside quoted text.
func stripComments(query string) string {
	var out strings.Builder
	for i := 0; i < len(query); i++ {
		switch c := query[i]; {
		case c == '\'' || c == '"' || c == '`':
			end := strings.IndexByte(query[i+1:], c)
			if end < 0 {
				out.WriteString(query[i:])
				return out.String()
			}
			out.WriteString(query[i : i+end+2])
			i += end + 1
		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			for i < len(query) && query[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
		case c == '/' && i+1 < len(query) && query[i+1] == '*':
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return out.String()
			}
			i += end + 3
			out.WriteByte(' ')
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// readStatement reports a statement known to read. Everything else counts as
// a write: an unknown shape is checked or refused rather than let through.
// A PRAGMA with a value, as an assignment or in parentheses, may set
// something or do work (user_version(5), wal_checkpoint(TRUNCATE)), except
// the few that take a table or index name and only describe it; text with a
// second statement could hold anything.
func readStatement(query string) bool {
	if !singleStatement(query) {
		return false
	}
	switch statementVerb(query) {
	case "SELECT", "VALUES", "EXPLAIN":
		return true
	case "PRAGMA":
		text := stripComments(query)
		if strings.Contains(text, "=") {
			return false
		}
		name, _, argument := strings.Cut(text, "(")
		if !argument {
			return true
		}
		fields := strings.Fields(name)
		return len(fields) == 2 && describingPragmas[strings.ToLower(fields[1])]
	}
	return false
}

// describingPragmas take a table or index name in parentheses and only
// describe it. The migrations read table_info this way.
var describingPragmas = map[string]bool{
	"table_info": true, "table_xinfo": true, "index_list": true, "index_info": true,
	"index_xinfo": true, "foreign_key_list": true,
}

// savepointStatement reports the statements GORM issues for a nested
// transaction. They change no row, so they do not start the check.
func savepointStatement(query string) bool {
	if !singleStatement(query) {
		return false
	}
	switch statementVerb(query) {
	case "SAVEPOINT", "RELEASE", "ROLLBACK":
		return true
	}
	return false
}
