// Package uxstore keeps the gateway's per-request records (uxrecord.Request)
// in their own SQLite file (ux.db), separate from the main database and the
// log store, with the same shape the log store uses: one writer connection, a
// small reader pool, WAL, per-UTC-day partition tables and retention by
// dropping whole partitions.
//
// Each day has three tables:
//
//   - req_YYYYMMDD: the request log, one typed column per record field plus
//     the sampling weight. Under load the log is sampled (see sampler).
//   - grid_YYYYMMDD: the request-shape distributions, a count per
//     (5-minute bucket, deployment, kind, x, y), built from every request.
//   - cnt_YYYYMMDD: per (5-minute bucket, deployment) how many requests there
//     were and how many carried usage and a cache figure, from every request,
//     so a distribution can say how much of the traffic it covers.
//
// Observe never blocks the gateway: records go through a bounded queue to one
// background goroutine that aggregates them in memory and writes one
// transaction per flush. A full queue drops the record and counts it.
package uxstore

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	// Registers the pure-Go "sqlite" driver.
	_ "github.com/glebarez/go-sqlite"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Defaults for the zero fields of Options.
const (
	DefaultRowsPerSecond = 300
	DefaultMaxAge        = 7 * 24 * time.Hour
	DefaultMaxBytes      = 2 << 30
	// DefaultQueue holds several seconds of records at the fleet's request
	// rate (about 1300/s at 1000 nodes); the channel is allocated whole, so it
	// is not larger than that.
	DefaultQueue      = 8192
	DefaultFlushEvery = time.Second
)

// schemaVersion is stored in PRAGMA user_version. A file written by a newer
// schema is refused rather than misread; an older one is migrated (migrate.go).
const schemaVersion = 2

// retainEvery is how often the writer checks retention.
const retainEvery = time.Minute

// maxPendingRows flushes early when this many request rows wait in memory, so
// an anomaly storm (every request kept) cannot grow the buffer for a whole
// flush interval.
const maxPendingRows = 4096

// Options configures a Store. Zero fields take the defaults above.
type Options struct {
	// RowsPerSecond is the request-log budget: rows kept per wall-clock second
	// before normal requests start being sampled.
	RowsPerSecond int
	// MaxAge drops a day's partitions once the whole day is older than this.
	MaxAge time.Duration
	// MaxBytes bounds the live database size; the oldest days are dropped
	// (never the current one) while it is exceeded.
	MaxBytes int64
	// Queue is the capacity of the Observe queue.
	Queue int
	// FlushEvery is the interval between write transactions.
	FlushEvery time.Duration
	// Now is the clock; tests inject one.
	Now func() time.Time

	// beforeRecord, when set, runs on the writer goroutine before each
	// dequeued record is handled (tests use it to hold the writer).
	beforeRecord func()
}

func (o Options) withDefaults() Options {
	if o.RowsPerSecond <= 0 {
		o.RowsPerSecond = DefaultRowsPerSecond
	}
	if o.MaxAge <= 0 {
		o.MaxAge = DefaultMaxAge
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Queue <= 0 {
		o.Queue = DefaultQueue
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = DefaultFlushEvery
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Stats is a snapshot of the store's counters since Open.
type Stats struct {
	// Observed counts records handed to Observe (including dropped ones).
	Observed uint64
	// Dropped counts records lost because the queue was full or the store
	// was closed: requests the monitor is blind to.
	Dropped uint64
	// RowsWritten counts request-log rows committed.
	RowsWritten uint64
	// SampledAway counts normal requests the sampler did not keep a row for
	// (they are still in the grids and the coverage counts).
	SampledAway uint64
	// FlushErrors counts failed write transactions; their data is lost.
	FlushErrors uint64
	// RetainErrors counts failed retention passes (retried a minute later).
	RetainErrors uint64
	// Partitions is the number of days with tables.
	Partitions int64
	// PartitionsDropped counts days removed by retention.
	PartitionsDropped uint64
}

// Store is the request store.
type Store struct {
	opts   Options
	writer *sql.DB // the single connection SQLite allows to write
	reader *sql.DB // searches do not queue behind the writer under WAL

	queue chan uxrecord.Request
	ctl   chan ctlReq
	stop  chan struct{}
	done  chan struct{}

	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error

	observed, dropped, rowsWritten, sampledAway atomic.Uint64
	flushErrors, retainErrors, partsDropped     atomic.Uint64
	partitions                                  atomic.Int64

	// Writer-goroutine state.
	w writerState
}

// ctlReq asks the writer goroutine to flush now (and optionally run
// retention); done is closed once it has.
type ctlReq struct {
	retain bool
	done   chan error
}

// Open opens (creating if necessary) the store at path and starts its writer.
func Open(path string, opts Options) (*Store, error) {
	opts = opts.withDefaults()
	writer, err := openPooled(path, 1)
	if err != nil {
		return nil, err
	}
	reader, err := openPooled(path, 4)
	if err != nil {
		writer.Close()
		return nil, err
	}
	s := &Store{
		opts:   opts,
		writer: writer,
		reader: reader,
		queue:  make(chan uxrecord.Request, opts.Queue),
		ctl:    make(chan ctlReq),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	err = s.initSchema()
	if err == nil {
		err = s.initWriter()
	}
	if err != nil {
		writer.Close()
		reader.Close()
		return nil, err
	}
	go s.run()
	return s, nil
}

func openPooled(path string, conns int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(conns)
	// WAL keeps readers and the single writer from blocking each other.
	// synchronous=NORMAL may lose the last transactions on an OS crash, which
	// is acceptable for request observations.
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

// initSchema refuses a file written by a newer schema and brings an older
// one (or a new, empty one) to this version: the migrations and the version
// stamp commit together, so a failed migration leaves the file as it was.
// Tables are per-day and created on first write, so a current file has
// nothing else to do.
func (s *Store) initSchema() error {
	var v int
	if err := s.writer.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	switch {
	case v > schemaVersion:
		return fmt.Errorf("uxstore: file schema version %d is newer than %d", v, schemaVersion)
	case v < schemaVersion:
		return migrate(s.writer, v)
	}
	return nil
}

// Observe hands a finished request to the store. It never blocks: when the
// queue is full, or the store is closed, the record is dropped and counted.
func (s *Store) Observe(r uxrecord.Request) {
	s.observed.Add(1)
	if s.closed.Load() {
		s.dropped.Add(1)
		return
	}
	select {
	case s.queue <- r:
	default:
		s.dropped.Add(1)
	}
}

// Dropped is the number of records lost to a full queue or a closed store.
func (s *Store) Dropped() uint64 { return s.dropped.Load() }

// Stats returns the store's counters.
func (s *Store) Stats() Stats {
	return Stats{
		Observed:          s.observed.Load(),
		Dropped:           s.dropped.Load(),
		RowsWritten:       s.rowsWritten.Load(),
		SampledAway:       s.sampledAway.Load(),
		FlushErrors:       s.flushErrors.Load(),
		RetainErrors:      s.retainErrors.Load(),
		Partitions:        s.partitions.Load(),
		PartitionsDropped: s.partsDropped.Load(),
	}
}

// errClosed is returned by sync after Close.
var errClosed = errors.New("uxstore: closed")

// sync makes the writer drain the queue and flush (and run retention when
// asked), and waits for it. Tests use it in place of the flush ticker.
func (s *Store) sync(retain bool) error {
	req := ctlReq{retain: retain, done: make(chan error, 1)}
	select {
	case s.ctl <- req:
		return <-req.done
	case <-s.done:
		return errClosed
	}
}

// Close stops the writer after it drains the queue and flushes what it holds,
// then closes the database. Records observed concurrently with Close may be
// dropped (and counted). Close is idempotent.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		close(s.stop)
		<-s.done
		// Records that raced in after the writer's last drain are counted
		// as dropped, not lost silently.
		for len(s.queue) > 0 {
			<-s.queue
			s.dropped.Add(1)
		}
		errW := s.writer.Close()
		errR := s.reader.Close()
		s.closeErr = errors.Join(s.w.finalErr, errW, errR)
	})
	return s.closeErr
}
