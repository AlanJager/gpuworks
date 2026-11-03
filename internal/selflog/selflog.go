// Package selflog bridges a process's own slog output into the log center.
//
// A Collector wraps the handler the process already uses (stderr, rotated
// file): every record at or above a minimum level is additionally pushed into
// a bounded queue for a best-effort secondary consumer, while the wrapped
// handler still sees every record exactly as before. The queue bound and the
// drop counter exist so a slow consumer can never slow logging down or grow
// memory — losing self-log lines under pressure is better than losing the
// logger.
package selflog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// Record is one captured self-log line, already flattened for the logstore.
type Record struct {
	Level  string // debug|info|warn|error|fatal
	TimeMS int64
	Line   string // message plus key=value attrs in log order
}

// Collector is an slog.Handler middleware with a bounded queue.
type Collector struct {
	inner   slog.Handler
	min     slog.Level
	queue   chan Record
	dropped *atomic.Int64
	// pre attrs/groups from WithAttrs/WithGroup, re-applied when flattening a
	// record into a line (the slog.Record itself does not carry them).
	pre   []slog.Attr
	group string
}

// NewCollector wraps inner. Records at or above min are queued (capacity
// queue, overflow dropped and counted); every record is also passed to inner.
func NewCollector(inner slog.Handler, min slog.Level, queue int) *Collector {
	if queue < 1 {
		queue = 1
	}
	return &Collector{
		inner:   inner,
		min:     min,
		queue:   make(chan Record, queue),
		dropped: &atomic.Int64{},
	}
}

// Enabled reports whether either side wants the record: the queue (at or
// above min) or the wrapped handler at its own level.
func (c *Collector) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= c.min || c.inner.Enabled(ctx, level)
}

func (c *Collector) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= c.min {
		var b strings.Builder
		b.WriteString(c.group)
		b.WriteString(r.Message)
		for _, a := range c.pre {
			fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
		}
		r.Attrs(func(a slog.Attr) bool {
			fmt.Fprintf(&b, " %s=%s", a.Key, a.Value)
			return true
		})
		rec := Record{Level: levelName(r.Level), TimeMS: time.Now().UnixMilli(), Line: b.String()}
		select {
		case c.queue <- rec:
		default:
			c.dropped.Add(1)
		}
	}
	// A handler's Handle does not check its level - slog.Logger asks Enabled
	// first - so a record the queue wanted must not reach a wrapped handler
	// set above it.
	if !c.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return c.inner.Handle(ctx, r)
}

// WithAttrs returns a derived handler sharing this collector's queue.
func (c *Collector) WithAttrs(attrs []slog.Attr) slog.Handler {
	pre := make([]slog.Attr, 0, len(c.pre)+len(attrs))
	pre = append(pre, c.pre...)
	pre = append(pre, attrs...)
	return &Collector{inner: c.inner.WithAttrs(attrs), min: c.min, queue: c.queue, dropped: c.dropped, pre: pre, group: c.group}
}

// WithGroup returns a derived handler sharing this collector's queue; group
// names become a line prefix.
func (c *Collector) WithGroup(name string) slog.Handler {
	group := name
	if c.group != "" {
		group = c.group + "." + name
	}
	return &Collector{inner: c.inner.WithGroup(name), min: c.min, queue: c.queue, dropped: c.dropped, pre: c.pre, group: group + ": "}
}

// Drain pops up to max queued records, oldest first, without blocking.
func (c *Collector) Drain(max int) []Record {
	if max < 1 {
		return nil
	}
	out := make([]Record, 0, max)
	for len(out) < max {
		select {
		case rec := <-c.queue:
			out = append(out, rec)
		default:
			return out
		}
	}
	return out
}

// Dropped reports how many records were discarded because the queue was full.
func (c *Collector) Dropped() int64 { return c.dropped.Load() }

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warn"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}
