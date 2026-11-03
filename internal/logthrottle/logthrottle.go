// Package logthrottle keeps a burst of identical log records from flooding a
// process's logs, the way journald rate-limits a noisy service but per kind
// of message: within each window a kind of record is written up to a limit,
// the rest are counted, and one summary record says how many were suppressed.
//
// A kind is, for info and above, the level, the message, the logger's own
// attributes (slog.Logger.With) and the attributes named as identity (an
// error code, say): "api error code=X" from many clients is one kind, while a
// different code is another. A debug record's kind is the message and every
// attribute value, so only exact repeats are suppressed - debug output exists
// to show each distinct step, and its volume is bounded elsewhere.
package logthrottle

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Options configure a Handler. Zero values take the defaults.
type Options struct {
	// Window is the counting window (default 30s).
	Window time.Duration
	// Limit is how many records of one kind at info and above are written
	// per window (default 10); DebugLimit the same for debug (default 50).
	Limit      int
	DebugLimit int
	// Identity names the record attributes that make two otherwise equal
	// records different kinds.
	Identity []string
	// MaxKinds bounds the kinds tracked at once (default 4096). Beyond it a
	// record of a new kind is counted by its level and message alone.
	MaxKinds int
	// Now is the clock; tests set it.
	Now func() time.Time
}

// SuppressedMessage is the summary record's message.
const SuppressedMessage = "suppressed similar log messages"

// Handler is an slog.Handler middleware.
type Handler struct {
	inner slog.Handler
	state *state
	// prefix is the kind key of the logger's own attributes and groups.
	prefix string
}

type state struct {
	opts     Options
	identity map[string]bool
	mu       sync.Mutex
	kinds    map[string]*kind
	swept    time.Time
}

type kind struct {
	// inner is the handler (with the logger's attributes) the summary goes to.
	inner      slog.Handler
	level      slog.Level
	message    string
	identity   []slog.Attr
	start      time.Time
	count      int
	suppressed int
}

// New wraps inner.
func New(inner slog.Handler, opts Options) *Handler {
	if opts.Window <= 0 {
		opts.Window = 30 * time.Second
	}
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	if opts.DebugLimit <= 0 {
		opts.DebugLimit = 50
	}
	if opts.MaxKinds <= 0 {
		opts.MaxKinds = 4096
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	identity := make(map[string]bool, len(opts.Identity))
	for _, key := range opts.Identity {
		identity[key] = true
	}
	return &Handler{inner: inner, state: &state{opts: opts, identity: identity, kinds: map[string]*kind{}}}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	s := h.state
	buf := keyBuffers.Get().(*[]byte)
	key := h.appendKey((*buf)[:0], r)
	now := s.opts.Now()
	limit := s.opts.Limit
	if r.Level < slog.LevelInfo {
		limit = s.opts.DebugLimit
	}

	s.mu.Lock()
	summaries := s.sweepLocked(now)
	k := s.kinds[string(key)] // no allocation for the lookup
	if k == nil && len(s.kinds) >= s.opts.MaxKinds {
		// Too many kinds at once (a flood of distinct paths, say): the record
		// counts against its message alone, so the bound cannot be walked
		// around. These coarse kinds are few - one per message.
		key = append(h.appendCoarseKey(key[:0], r), 0, '*')
		k = s.kinds[string(key)]
		if k == nil {
			k = &kind{inner: h.inner, level: r.Level, message: r.Message, start: now}
			s.kinds[string(key)] = k
		}
	}
	if k == nil {
		k = &kind{inner: h.inner, level: r.Level, message: r.Message, identity: h.identityOf(r), start: now}
		s.kinds[string(key)] = k
	}
	if now.Sub(k.start) >= s.opts.Window {
		if k.suppressed > 0 {
			summaries = append(summaries, k.summary(now))
		}
		k.start, k.count, k.suppressed = now, 0, 0
	}
	write := true
	k.count++
	if k.count > limit {
		k.suppressed++
		write = false
	}
	s.mu.Unlock()
	*buf = key
	keyBuffers.Put(buf)

	for _, sum := range summaries {
		_ = sum.inner.Handle(ctx, sum.record)
	}
	if !write {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

// Sweep writes the summaries of windows that have ended and forgets kinds
// that have gone quiet. Handle sweeps as records arrive; a process calls
// Sweep on a timer (Run) so a flood that stops still gets its summary.
func (h *Handler) Sweep(ctx context.Context) {
	h.state.mu.Lock()
	summaries := h.state.sweepLocked(h.state.opts.Now())
	h.state.mu.Unlock()
	for _, sum := range summaries {
		_ = sum.inner.Handle(ctx, sum.record)
	}
}

// Run sweeps once per window until ctx ends.
func (h *Handler) Run(ctx context.Context) {
	ticker := time.NewTicker(h.state.opts.Window)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.Sweep(ctx)
		}
	}
}

type pendingSummary struct {
	inner  slog.Handler
	record slog.Record
}

// sweepLocked runs at most once per window: kinds whose window has ended are
// summarized (when they suppressed anything) and dropped.
func (s *state) sweepLocked(now time.Time) []pendingSummary {
	if now.Sub(s.swept) < s.opts.Window {
		return nil
	}
	s.swept = now
	var out []pendingSummary
	for key, k := range s.kinds {
		if now.Sub(k.start) < s.opts.Window {
			continue
		}
		if k.suppressed > 0 {
			out = append(out, k.summary(now))
		}
		delete(s.kinds, key)
	}
	return out
}

func (k *kind) summary(now time.Time) pendingSummary {
	r := slog.NewRecord(now, k.level, SuppressedMessage, 0)
	r.AddAttrs(slog.String("message", k.message), slog.Int("suppressed", k.suppressed),
		slog.Time("since", k.start), slog.Duration("window", now.Sub(k.start)))
	r.AddAttrs(k.identity...)
	return pendingSummary{inner: k.inner, record: r}
}

var keyBuffers = sync.Pool{New: func() any { b := make([]byte, 0, 256); return &b }}

// appendCoarseKey appends the record's level, message and logger attributes.
func (h *Handler) appendCoarseKey(b []byte, r slog.Record) []byte {
	b = append(b, r.Level.String()...)
	b = append(b, 0)
	b = append(b, r.Message...)
	b = append(b, 0)
	return append(b, h.prefix...)
}

// appendKey appends the record's kind to b.
func (h *Handler) appendKey(b []byte, r slog.Record) []byte {
	b = h.appendCoarseKey(b, r)
	debug := r.Level < slog.LevelInfo
	r.Attrs(func(a slog.Attr) bool {
		if debug || h.state.identity[a.Key] {
			b = appendAttr(b, a)
		}
		return true
	})
	return b
}

// identityOf is the record's identity attributes, which its kind's summary
// repeats.
func (h *Handler) identityOf(r slog.Record) []slog.Attr {
	var out []slog.Attr
	if r.Level < slog.LevelInfo {
		return nil
	}
	r.Attrs(func(a slog.Attr) bool {
		if h.state.identity[a.Key] {
			out = append(out, a)
		}
		return true
	})
	return out
}

func appendAttr(b []byte, a slog.Attr) []byte {
	b = append(b, 0)
	b = append(b, a.Key...)
	b = append(b, '=')
	v := a.Value.Resolve()
	if v.Kind() == slog.KindString {
		return append(b, v.String()...)
	}
	return fmt.Append(b, v.Any())
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	b := []byte(h.prefix)
	for _, a := range attrs {
		b = appendAttr(b, a)
	}
	return &Handler{inner: h.inner.WithAttrs(attrs), state: h.state, prefix: string(b)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{inner: h.inner.WithGroup(name), state: h.state, prefix: h.prefix + "\x00group=" + name}
}
