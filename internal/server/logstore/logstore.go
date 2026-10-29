// Package logstore is an embedded log store with full-text search, backed by
// pure-Go SQLite (github.com/glebarez/go-sqlite, FTS5 compiled in) in its own
// database file (logs.db), separate from the main application database.
//
// Rows live in per-day partitions: one plain table holding the searchable
// columns plus indexes (ts, instance, node, level, source) and one
// external-content FTS5 table indexing the line text. Partition iteration is
// newest-day-first with early exit, which is what keeps first-screen and
// keyword queries fast as the corpus grows; retention drops whole expired
// partitions instead of deleting rows.
package logstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"

	// Registers the pure-Go "sqlite" driver (modernc). FTS5 is compiled in.
	_ "github.com/glebarez/go-sqlite"
)

// Severity levels as produced by the agent's classifier. Empty means unknown.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelFatal = "fatal"
)

// Log sources. The default is instance (container logs); node/agent/server are
// infrastructure logs the UI keeps in a separate group.
const (
	SourceInstance = "instance"
	SourceNode     = "node"
	SourceAgent    = "agent"
	SourceServer   = "server"
)

// Entry is a single log line.
type Entry struct {
	// ID is the rowid inside the entry's day partition. Together with TimeMS
	// it is the pagination cursor identity; it is not globally unique.
	ID       int64  `json:"ID"`
	Instance string `json:"Instance"`
	Node     string `json:"Node"`
	Level    string `json:"Level"`
	Source   string `json:"Source"`
	TimeMS   int64  `json:"TimeMS"`
	Line     string `json:"Line"`
}

// MaxInstances bounds Filter.Instances: an IN-list beyond this size is
// rejected rather than compiled, the same reasoning as TotalCap - a
// service-page "view logs for this deployment" link naturally caps out at a
// deployment's own replica count, so a much larger list signals a caller
// bug rather than a legitimate query.
const MaxInstances = 64

// Filter selects log lines. Zero-value fields mean "no constraint".
type Filter struct {
	Query    string // FTS5 syntax; "" = recent tail
	Instance string // exact match or a prefix (>= 4 chars recommended)
	// Instances selects an explicit set of instances by exact UUID (no
	// prefix matching, unlike Instance) instead of one - the service page's
	// merged log stream across a deployment's replicas. Mutually exclusive
	// with Instance and capped at MaxInstances; Search and Histogram return
	// an error when either rule is violated. An empty (nil or zero-length)
	// Instances behaves exactly like leaving it unset. Context stays
	// single-instance and never consults this field.
	//
	// Measured cost: an IN(64) predicate materializes every matching row of
	// those instances within a day's partition before LIMIT is applied
	// (~1s for a 320k-row partition) - the (instance, ts, id) index serves
	// the IN() lookup itself, but ORDER BY m.id still needs a temp b-tree
	// because that index is not already ordered by id across multiple
	// instance values the way it is for a single Instance. Fine for a
	// deployment's own replica count; not a substitute for a cheap
	// fleet-wide scan.
	Instances []string
	Node      string
	Level     string // minimum severity (debug < info < warn < error < fatal)
	Source    string // exact source
	FromMS    int64  // inclusive
	ToMS      int64  // inclusive
	// CursorTS/CursorID form the keyset cursor: rows strictly older than the
	// pair are returned. Use the last row of the previous page.
	CursorTS int64
	CursorID int64
	// Limit bounds the page Search returns. <=0 resolves to DefaultLimit;
	// anything above MaxLimit is clamped to it.
	Limit     int
	WithTotal bool // populate Result.Total (bounded by TotalCap)
}

// DefaultLimit is what an unset (<=0) Filter.Limit resolves to.
// MaxLimit is the largest page Search will ever return in one call,
// regardless of what the caller asks for.
const (
	DefaultLimit = 1000
	MaxLimit     = 5000
)

// Result is a page of entries, newest-first.
type Result struct {
	Entries []Entry
	// Truncated is true when more rows matched than Entries carries (Search
	// probed one row past the effective limit internally) - the caller
	// asked for a page smaller than the full match set, not that the store
	// hit an error or a hard ceiling.
	Truncated   bool
	Total       int64 // only when Filter.WithTotal
	TotalCapped bool  // counting stopped at TotalCap
}

// TotalCap bounds WithTotal counting so a broad match cannot turn into an
// unbounded scan across every partition.
const TotalCap = 1_000_000

// Bucket aggregates one time bucket of a histogram by severity.
type Bucket struct {
	StartMS int64
	Counts  map[string]int64 // level -> rows; absent levels are zero
}

// Store is an append-and-search log store.
type Store interface {
	Append(entries []Entry) error
	Search(filter Filter) (Result, error)
	// Tail returns the most recent entries of an instance (no query),
	// oldest-first, like reading a log file top-to-bottom.
	Tail(instance string, limit int) ([]Entry, error)
	// Context returns the window of lines around a match, chronological, with
	// matchIndex pointing at the matched row inside the returned slice, or -1
	// when the anchor id is absent from the window's partition (pruned by
	// retention, or a ts from a different day than the id). The window is
	// scoped to the same instance when the row has one, otherwise to the same
	// node (infrastructure logs), and never crosses a day boundary.
	Context(instance, node string, ts, id int64, before, after int) (rows []Entry, matchIndex int, err error)
	// Histogram counts rows per bucket (unix-ms aligned) and severity for the
	// filter, over the partitions the filter's time range touches.
	Histogram(filter Filter, bucketMS int64) ([]Bucket, error)
	// Retain drops expired logs. It honours ctx between partitions/batches so
	// a caller shutting down is not held up by a large backlog.
	Retain(ctx context.Context, olderThanMS int64) error
	// Checkpoint advances the write-ahead log on the writer connection.
	Checkpoint() (busy bool, logPages, checkpointed int64, err error)
	Close() error
}

type sqliteStore struct {
	// writer holds the single connection SQLite allows to write; reader is a
	// small pool so searches do not queue behind ingest under WAL.
	writer *sql.DB
	reader *sql.DB
	// partitionsReady memoizes days whose DDL ensurePartition already ran.
	// Append can then skip ~10 IF-NOT-EXISTS catalog checks per batch (at
	// fleet ingest that was thousands of no-op DDLs per second); Retain
	// invalidates entries for the days it drops so a late-arriving row for a
	// dropped day recreates its partition instead of failing the insert.
	partitionsReady sync.Map
}

// OpenSQLite opens (creating if necessary) the log store at path. Any legacy
// single-table layout found in the file is migrated atomically on open.
func OpenSQLite(path string) (Store, error) {
	writer, err := openPooled(path, 1)
	if err != nil {
		return nil, err
	}
	reader, err := openPooled(path, 4)
	if err != nil {
		writer.Close()
		return nil, err
	}
	s := &sqliteStore{writer: writer, reader: reader}
	if err := s.migrateLegacy(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate legacy logstore: %w", err)
	}
	return s, nil
}

func openPooled(path string, conns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(conns)
	// WAL keeps readers and the single writer from blocking each other.
	// synchronous=NORMAL trades last-transactions durability on OS crash for
	// ingest speed — acceptable for diagnostic logs, the agent re-syncs on
	// reconnect anyway.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return db, nil
}

func (s *sqliteStore) Close() error {
	errW := s.writer.Close()
	errR := s.reader.Close()
	if errW != nil {
		return errW
	}
	return errR
}

// levelRank orders severities for the >= filter semantics. Unknown level
// (empty) ranks below everything, so level-filtered queries exclude it.
var levelRank = map[string]int{
	LevelDebug: 0,
	LevelInfo:  1,
	LevelWarn:  2,
	LevelError: 3,
	LevelFatal: 4,
}

func rankOf(level string) int { return levelRank[level] }

// validate rejects filter combinations the store cannot execute: Instance
// (exact/prefix match) and Instances (an IN list) are mutually exclusive,
// and Instances is capped at MaxInstances so a query can never turn into an
// unbounded IN-list scan. Search and Histogram both call this before
// touching any partition.
func (f Filter) validate() error {
	if f.Instance != "" && len(f.Instances) > 0 {
		return fmt.Errorf("logstore: Filter.Instance and Filter.Instances are mutually exclusive")
	}
	if len(f.Instances) > MaxInstances {
		return fmt.Errorf("logstore: Filter.Instances has %d entries, exceeds the %d limit", len(f.Instances), MaxInstances)
	}
	return nil
}

// quotePhrase wraps a raw user string as a single FTS5 phrase, escaping the
// embedded double quotes per FTS5 rules ("" is a literal quote). This makes
// any input syntactically valid, at the cost of treating it as a literal
// phrase — the fallback when a raw query turns out to be invalid syntax.
func quotePhrase(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
