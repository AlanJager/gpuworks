// Package debuglog adds a time- and size-bounded debug session to a process's
// logging: while a session is open, every record at debug and above is also
// written to the session's own file, and the process's usual handler (its log
// file at its configured level, the log center) is left exactly as it was. A
// session cannot crowd the regular log out of its rotation, and it ends by
// itself - at its deadline, or when its file reaches its size limit.
package debuglog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Tee is the slog.Handler middleware: inner is the process's handler.
type Tee struct {
	inner  slog.Handler
	shared *shared
	// pre are the logger's own attributes, added to a record written to the
	// session file (the session handler is opened after the logger exists).
	pre []slog.Attr
}

type shared struct {
	mu sync.Mutex
	// session is the open session (nil when none, so a record outside a
	// session costs one atomic load); last is the latest one, open or not.
	session atomic.Pointer[session]
	last    atomic.Pointer[session]
	now     func() time.Time
}

type session struct {
	owner   *shared
	id      string
	path    string
	until   time.Time
	limit   int64
	file    *os.File
	handler slog.Handler
	written atomic.Int64
	mu      sync.Mutex // serializes writes and the close
	closed  bool
	reason  string
}

// Status describes the open session, or the last one when none is open.
type Status struct {
	ID      string    `json:"id"`
	Path    string    `json:"path"`
	Until   time.Time `json:"until"`
	Limit   int64     `json:"limit_bytes"`
	Written int64     `json:"written_bytes"`
	Open    bool      `json:"open"`
	// Ended says why a closed session ended: "deadline", "size_limit" or
	// "stopped".
	Ended string `json:"ended,omitempty"`
}

// New wraps inner.
func New(inner slog.Handler) *Tee {
	return &Tee{inner: inner, shared: &shared{now: time.Now}}
}

// SetClock replaces the clock; tests use it.
func (t *Tee) SetClock(now func() time.Time) { t.shared.now = now }

// Start opens a session writing to path until the deadline or limit bytes,
// replacing any open session (which ends as "stopped").
func (t *Tee) Start(id, path string, until time.Time, limit int64) error {
	if limit <= 0 {
		return fmt.Errorf("debug log limit must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s := &session{owner: t.shared, id: id, path: path, until: until, limit: limit, file: f}
	s.handler = slog.NewTextHandler(&limitedWriter{s: s}, &slog.HandlerOptions{Level: slog.LevelDebug})
	t.shared.mu.Lock()
	previous := t.shared.session.Swap(s)
	t.shared.last.Store(s)
	t.shared.mu.Unlock()
	if previous != nil {
		previous.close("stopped")
	}
	return nil
}

// Stop ends the session id (any session when id is empty); it reports whether
// one was open.
func (t *Tee) Stop(id string) bool {
	s := t.shared.session.Load()
	if s == nil || id != "" && s.id != id {
		return false
	}
	s.close("stopped")
	return true
}

// Status reports the current or last session; ok is false before the first.
func (t *Tee) Status() (Status, bool) {
	s := t.shared.last.Load()
	if s == nil {
		return Status{}, false
	}
	t.expire(s)
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{ID: s.id, Path: s.path, Until: s.until, Limit: s.limit, Written: s.written.Load(), Open: !s.closed, Ended: s.reason}, true
}

// active is the open session, closing one past its deadline.
func (t *Tee) active() *session {
	s := t.shared.session.Load()
	if s == nil {
		return nil
	}
	if t.expire(s) {
		return nil
	}
	return s
}

func (t *Tee) expire(s *session) bool {
	if !t.shared.now().Before(s.until) {
		s.close("deadline")
		return true
	}
	return false
}

func (t *Tee) Enabled(ctx context.Context, level slog.Level) bool {
	return t.inner.Enabled(ctx, level) || t.active() != nil
}

func (t *Tee) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if t.inner.Enabled(ctx, r.Level) {
		err = t.inner.Handle(ctx, r)
	}
	if s := t.active(); s != nil {
		if len(t.pre) > 0 {
			r = r.Clone()
			r.AddAttrs(t.pre...)
		}
		_ = s.handler.Handle(ctx, r)
	}
	return err
}

func (t *Tee) WithAttrs(attrs []slog.Attr) slog.Handler {
	pre := make([]slog.Attr, 0, len(t.pre)+len(attrs))
	pre = append(append(pre, t.pre...), attrs...)
	return &Tee{inner: t.inner.WithAttrs(attrs), shared: t.shared, pre: pre}
}

// WithGroup keeps the group for the process's handler; the session file sees
// the record's attributes ungrouped.
func (t *Tee) WithGroup(name string) slog.Handler {
	return &Tee{inner: t.inner.WithGroup(name), shared: t.shared, pre: t.pre}
}

func (s *session) close(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed, s.reason = true, reason
	s.owner.session.CompareAndSwap(s, nil)
	_, _ = fmt.Fprintf(s.file, "time=%s msg=%q reason=%s written_bytes=%d\n",
		time.Now().UTC().Format(time.RFC3339Nano), "debug session ended", reason, s.written.Load())
	_ = s.file.Close()
}

// limitedWriter writes a session's records until its limit; the record that
// would cross it closes the session instead.
type limitedWriter struct{ s *session }

func (w *limitedWriter) Write(p []byte) (int, error) {
	s := w.s
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return len(p), nil
	}
	if s.written.Load()+int64(len(p)) > s.limit {
		s.mu.Unlock()
		s.close("size_limit")
		return len(p), nil
	}
	n, err := s.file.Write(p)
	s.written.Add(int64(n))
	s.mu.Unlock()
	return n, err
}

// Sweep removes files under dir older than maxAge: finished sessions' files
// are kept for collection a while, not forever.
func Sweep(dir string, maxAge time.Duration, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < maxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}
