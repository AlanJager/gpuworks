package pluginhost

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
)

// A plugin that stops reading its serve connection stalls every call on
// it: HTTP/2 shares one connection-level flow-control window across the
// calls, so request bodies the plugin does not read use it up, and once
// the socket's buffer is full even the frames of other calls block. Its
// health probes may still pass (an SDK answers them from another
// goroutine), so the supervisor watches the connection itself
// (docs/dev/extensions.md, 监管).
//
// stallMeter is that watch for one serve connection. Every counter is an
// atomic word so the per-call and per-write paths never allocate or take
// a lock shared across calls.
type stallMeter struct {
	// writing is when the connection writes in progress started (unix
	// nanoseconds), zero when none is; writers counts them.
	writing atomic.Int64
	writers atomic.Int32
	// unsent is the request body bytes the HTTP/2 client has read from
	// callers' bodies and not yet written (it holds them while it waits
	// for flow control); progress is when body bytes were last written
	// (a DATA frame reached the connection, or a held chunk was sent), or
	// when bytes became pending after none were.
	unsent   atomic.Int64
	progress atomic.Int64
	now      func() time.Time
}

func newStallMeter(now func() time.Time) *stallMeter {
	if now == nil {
		now = time.Now
	}
	return &stallMeter{now: now}
}

func (m *stallMeter) nanos() int64 { return m.now().UnixNano() }

// writeStarted and writeDone bracket one write to the connection.
func (m *stallMeter) writeStarted() {
	if m.writers.Add(1) == 1 {
		m.writing.Store(m.nanos())
	}
}

func (m *stallMeter) writeDone() {
	if m.writers.Add(-1) == 0 {
		m.writing.Store(0)
	}
}

// hold notes n body bytes the client read and has yet to write.
func (m *stallMeter) hold(n int64) {
	if m.unsent.Add(n) == n {
		m.progress.Store(m.nanos())
	}
}

// sent notes that n held bytes were written; dropped that they were
// abandoned (the call ended), which is not progress.
func (m *stallMeter) sent(n int64) {
	m.progress.Store(m.nanos())
	m.unsent.Add(-n)
}

func (m *stallMeter) dropped(n int64) { m.unsent.Add(-n) }

// stalled says whether the connection has made no progress for longer
// than after, and for how long: a write blocked that long, or request
// body bytes pending with none written that long.
func (m *stallMeter) stalled(after time.Duration) (time.Duration, bool) {
	now := m.nanos()
	var worst time.Duration
	if w := m.writing.Load(); w != 0 {
		worst = time.Duration(now - w)
	}
	if m.unsent.Load() > 0 {
		worst = max(worst, time.Duration(now-m.progress.Load()))
	}
	return worst, worst > after
}

// meteredConn reports every write to its meter, and every write that
// carries request body bytes (DATA frames) as progress: other frames,
// such as a health probe's, do not move bodies.
type meteredConn struct {
	net.Conn
	m    *stallMeter
	scan frameScan
}

func (c *meteredConn) Write(p []byte) (int, error) {
	c.m.writeStarted()
	n, err := c.Conn.Write(p)
	c.m.writeDone()
	if c.scan.data(p[:n]) > 0 {
		c.m.progress.Store(c.m.nanos())
	}
	return n, err
}

// frameScan follows the HTTP/2 frames a client writes, across writes,
// and counts the payload bytes of DATA frames. The client serializes its
// writes, so it needs no lock.
type frameScan struct {
	preface int // client preface bytes still to skip
	hdr     [frameHeaderLen]byte
	hn      int  // header bytes seen of the frame being read
	left    int  // payload bytes left of the current frame
	isData  bool // the current frame is DATA
}

const (
	frameHeaderLen = 9
	frameTypeData  = 0x0
)

func newFrameScan() frameScan { return frameScan{preface: len(http2.ClientPreface)} }

func (s *frameScan) data(p []byte) int {
	n := 0
	for len(p) > 0 {
		switch {
		case s.preface > 0:
			k := min(s.preface, len(p))
			s.preface -= k
			p = p[k:]
		case s.left > 0:
			k := min(s.left, len(p))
			if s.isData {
				n += k
			}
			s.left -= k
			p = p[k:]
		default:
			k := copy(s.hdr[s.hn:], p)
			s.hn += k
			p = p[k:]
			if s.hn == frameHeaderLen {
				s.hn = 0
				s.left = int(s.hdr[0])<<16 | int(s.hdr[1])<<8 | int(s.hdr[2])
				s.isData = s.hdr[3] == frameTypeData
			}
		}
	}
	return n
}

// meteredBody is a call's request body as the HTTP/2 client reads it. The
// client reads a chunk, then writes all of it (waiting for flow control
// as needed) before it reads again, so the chunk last read is held until
// the next read starts or the body is written out (finish).
type meteredBody struct {
	src io.ReadCloser
	m   *stallMeter

	mu       sync.Mutex
	held     int64
	finished bool
}

func (b *meteredBody) Read(p []byte) (int, error) {
	b.release(true)
	n, err := b.src.Read(p)
	if n > 0 {
		b.mu.Lock()
		if !b.finished {
			b.held += int64(n)
			b.m.hold(int64(n))
		}
		b.mu.Unlock()
	}
	return n, err
}

// release gives back the held chunk: written when sent, abandoned when not.
func (b *meteredBody) release(sent bool) {
	b.mu.Lock()
	n := b.held
	b.held = 0
	b.mu.Unlock()
	if n == 0 {
		return
	}
	if sent {
		b.m.sent(n)
	} else {
		b.m.dropped(n)
	}
}

// finish ends the accounting: sent says whether the body was written out.
// Reads after it hold nothing.
func (b *meteredBody) finish(sent bool) {
	b.mu.Lock()
	b.finished = true
	b.mu.Unlock()
	b.release(sent)
}

func (b *meteredBody) Close() error {
	b.finish(false)
	return b.src.Close()
}
