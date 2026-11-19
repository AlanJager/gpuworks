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
// (docs/dev/extensions.md, supervision).
//
// stallMeter is that watch for one serve connection. The pending count and
// its progress time change together under mu; all hot paths allocate nothing.
// Atomic fields also let diagnostics sample individual counters.
type stallMeter struct {
	mu sync.Mutex

	// writing is when the write in progress started (elapsed nanoseconds),
	// zero when none is. The HTTP/2 client serializes its writes to the
	// connection (under its write lock), so at most one is in progress.
	writing atomic.Int64
	// unsent is the request body bytes the HTTP/2 client has read from
	// callers' bodies and not yet written (it holds them while it waits
	// for flow control); progress is when body bytes were last written
	// (a DATA frame reached the connection, or a held chunk was sent), or
	// when bytes became pending after none were.
	unsent   atomic.Int64
	progress atomic.Int64
	now      func() int64
}

// now, when supplied by a test, returns nonzero monotonic elapsed nanoseconds.
func newStallMeter(now func() int64) *stallMeter {
	if now == nil {
		origin := time.Now()
		now = func() int64 { return time.Since(origin).Nanoseconds() + 1 }
	}
	return &stallMeter{now: now}
}

func (m *stallMeter) nanos() int64 { return m.now() }

// Progress never moves backward when a caller sampled its time and was then
// descheduled while another body made progress. Caller holds m.mu.
func (m *stallMeter) progressAt(now int64) {
	if now > m.progress.Load() {
		m.progress.Store(now)
	}
}

func (m *stallMeter) progressed() {
	now := m.nanos()
	m.mu.Lock()
	m.progressAt(now)
	m.mu.Unlock()
}

// writeStarted and writeDone bracket one write to the connection; writes
// do not overlap.
func (m *stallMeter) writeStarted() { m.writing.Store(m.nanos()) }

func (m *stallMeter) writeDone() { m.writing.Store(0) }

// hold notes n body bytes the client read and has yet to write. When none
// were pending, the wait starts now: the time is stored before the count,
// so a check that sees the bytes never pairs them with an older time.
func (m *stallMeter) hold(n int64) {
	now := m.nanos()
	m.mu.Lock()
	if m.unsent.Load() == 0 {
		m.progressAt(now)
	}
	m.unsent.Add(n)
	m.mu.Unlock()
}

// sent notes that n held bytes were written; dropped that they were
// abandoned (the call ended), which is not progress.
func (m *stallMeter) sent(n int64) {
	now := m.nanos()
	m.mu.Lock()
	m.progressAt(now)
	m.unsent.Add(-n)
	m.mu.Unlock()
}

func (m *stallMeter) dropped(n int64) {
	m.mu.Lock()
	m.unsent.Add(-n)
	m.mu.Unlock()
}

// stalled says whether the connection has made no progress for longer
// than after, and for how long: a write blocked that long, or request
// body bytes pending with none written that long.
func (m *stallMeter) stalled(after time.Duration) (time.Duration, bool) {
	now := m.nanos()
	m.mu.Lock()
	defer m.mu.Unlock()
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
		c.m.progressed()
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
// the next read starts or the body is written out (finish). One read is
// the exception: once a body of known length has been read whole, the
// client reads once more to see its end before it writes the last chunk,
// so that read releases nothing.
type meteredBody struct {
	src io.ReadCloser
	m   *stallMeter
	// size is the body's declared length, -1 when unknown; read counts the
	// bytes read so far. Only the client's single body reader uses them.
	size int64
	read int64

	mu       sync.Mutex
	held     int64
	finished bool
}

// newMeteredBody meters src, whose declared length is size (an outgoing
// request's ContentLength: 0 with a body means unknown).
func newMeteredBody(src io.ReadCloser, size int64, m *stallMeter) *meteredBody {
	if size <= 0 {
		size = -1
	}
	return &meteredBody{src: src, m: m, size: size}
}

func (b *meteredBody) Read(p []byte) (int, error) {
	if b.size < 0 || b.read < b.size {
		b.release(true)
	}
	n, err := b.src.Read(p)
	b.read += int64(n)
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
