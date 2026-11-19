package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Limits on a plugin's stderr (docs/dev/extensions.md): lines forwarded to
// the server log per second and in a burst, the longest line, and the
// bytes of recent output kept for diagnostics.
const (
	logLinesPerSecond = 50
	logLinesBurst     = 500
	maxLogLine        = 64 << 10
	stderrRingBytes   = 1 << 20
)

// ring keeps the last n bytes written to it, and counts every byte ever
// written (total), so a reader resumes at an offset and learns what it
// missed when the ring wrapped past it.
type ring struct {
	mu    sync.Mutex
	buf   []byte
	next  int
	full  bool
	total uint64
	// onWrite is called after every write, outside the lock; nil calls
	// nothing.
	onWrite func()
}

func newRing(n int) *ring { return &ring{buf: make([]byte, n)} }

func (r *ring) Write(p []byte) {
	r.mu.Lock()
	r.write(p)
	r.total += uint64(len(p))
	notify := r.onWrite
	r.mu.Unlock()
	if notify != nil && len(p) > 0 {
		notify()
	}
}

func (r *ring) write(p []byte) {
	if len(p) >= len(r.buf) {
		copy(r.buf, p[len(p)-len(r.buf):])
		r.next, r.full = 0, true
		return
	}
	n := copy(r.buf[r.next:], p)
	if n < len(p) {
		copy(r.buf, p[n:])
		r.full = true
	}
	r.next = (r.next + len(p)) % len(r.buf)
	if r.next == 0 && len(p) > 0 {
		r.full = true
	}
}

// Bytes returns the kept output, oldest first.
func (r *ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keptLocked()
}

func (r *ring) keptLocked() []byte {
	if !r.full {
		return append([]byte(nil), r.buf[:r.next]...)
	}
	return append(append([]byte(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}

// Since returns the output written from byte offset off on (offsets count
// every byte ever written), the offset after it, and whether the ring had
// already dropped some of it, in which case data starts at the oldest byte
// kept (from). An offset past the end is one of logs since forgotten: it
// reads the whole ring, as truncated.
func (r *ring) Since(off uint64) (data []byte, from, next uint64, truncated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kept := r.keptLocked()
	oldest := r.total - uint64(len(kept))
	switch {
	case off == r.total:
		return nil, r.total, r.total, false
	case off < oldest || off > r.total:
		return kept, oldest, r.total, true
	}
	return kept[off-oldest:], off, r.total, false
}

// forwardStderr copies a plugin's stderr into the ring and, within the
// rate limit, into the server log with plugin=<id>. A JSON line keeps its
// level, message and fields; any other line is logged at info. Lines over
// the limit are counted and the count logged once the flood ends.
func forwardStderr(r io.Reader, id string, log *slog.Logger, keep *ring, now func() time.Time) {
	limiter := newTokenBucket(logLinesPerSecond, logLinesBurst, now)
	br := bufio.NewReaderSize(r, 4096)
	dropped := 0
	for {
		line, err := readLine(br)
		if len(line) > 0 || err == nil {
			keep.Write(append(line, '\n'))
			if limiter.allow() {
				if dropped > 0 {
					log.Warn("plugin stderr lines dropped over the rate limit", "plugin", id, "dropped", dropped)
					dropped = 0
				}
				logLine(log, id, line)
			} else {
				dropped++
			}
		}
		if err != nil {
			if dropped > 0 {
				log.Warn("plugin stderr lines dropped over the rate limit", "plugin", id, "dropped", dropped)
			}
			return
		}
	}
}

// readLine reads one line without its newline, cut at maxLogLine.
func readLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(line) < maxLogLine {
			line = append(line, chunk[:min(len(chunk), maxLogLine-len(line))]...)
		}
		if err != nil || !isPrefix {
			return line, err
		}
	}
}

func logLine(log *slog.Logger, id string, line []byte) {
	var rec map[string]any
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &rec) != nil {
		log.Info(string(line), "plugin", id)
		return
	}
	msg, _ := rec["msg"].(string)
	level := slog.LevelInfo
	if s, ok := rec["level"].(string); ok {
		_ = level.UnmarshalText([]byte(strings.ToUpper(s)))
	}
	attrs := []slog.Attr{slog.String("plugin", id)}
	for k, v := range rec {
		switch k {
		case "msg", "level", "time", "plugin":
			continue
		}
		attrs = append(attrs, slog.Any(k, v))
	}
	log.LogAttrs(context.Background(), level, msg, attrs...)
}
