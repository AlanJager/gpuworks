package testenv

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"
)

// ShapedLink is a deterministic in-process TCP relay that puts a bandwidth,
// one-way-delay, and bounded-queue link between a client (the in-process agent)
// and a real server address. It exists to reproduce, offline, the link regimes
// the dual-lane tunnel work cares about:
//
//   - high bandwidth-delay product (e.g. 1 Gbps / 20 ms), where a small yamux
//     window caps single-stream throughput at window/RTT;
//   - a deep bufferbloat queue on a slow link (e.g. 10 Mbps / 16 MB queue),
//     where in-flight data ahead of a keepalive ping can stall it long enough
//     to tear a naively configured yamux session; and
//   - a slow link shared by the ctrl and data lanes (SharedRate), where the
//     control plane must stay responsive while the data lane is saturated.
//
// Each direction is shaped independently: a serialization delay enforces
// RateMbps, a delay line adds RTT/2 of latency, and a byte meter caps in-flight
// (queued-but-not-yet-delivered) bytes at QueueBytes. When the meter is full the
// relay stops reading from the source, so the source's socket buffer fills and
// the real writer behind it blocks — exactly the bufferbloat a deep router queue
// produces. Zero values mean "no limit / no delay".
//
// Every relayed connection is tagged with the lane named in the first bytes the
// client sends upstream (the WebSocket upgrade request line carries ?lane=ctrl
// or ?lane=data); DropLane severs only the connections of one lane.
type ShapedLink struct {
	rateMbps   float64
	rtt        time.Duration
	queueBytes int

	ln     net.Listener
	target string

	// Per-direction fair-share clocks, non-nil only with SharedRate. Index 0 is
	// client→server (up), 1 is server→client (down).
	shared [2]*fairClock

	mu     sync.Mutex
	conns  map[*connRec]struct{}
	closed bool
	doneCh chan struct{}

	wg sync.WaitGroup
}

// connRec is one relayed connection pair with its lane tag, the two per-direction
// queue meters (so a selective drop can release a pump parked on a full queue),
// and the per-direction serialization clocks used by the fair-share scheduler.
type connRec struct {
	client   net.Conn
	upstream net.Conn
	lane     string // "ctrl" | "data" | "" (single/legacy or untagged)
	meters   [2]*meter
	next     [2]time.Time // guarded by the owning fairClock's mutex
}

// ShapedLinkConfig configures a ShapedLink. A zero field disables that
// constraint (unlimited rate, no delay, unbounded queue).
type ShapedLinkConfig struct {
	RateMbps   float64
	RTT        time.Duration
	QueueBytes int
	// SharedRate makes RateMbps the aggregate capacity of the link, divided
	// fairly (per direction) among the connections that currently have data
	// backlogged, instead of a private cap per connection. This models N TCP
	// flows sharing one bottleneck: a saturating flow gets its fair share, and an
	// idle flow that sends a small message is served at once. Off by default.
	SharedRate bool
}

// NewShapedLink starts a shaped relay that forwards to target (host:port). It
// listens on 127.0.0.1:0 and begins accepting immediately; use Addr/URL to
// point a client at it. Close it to stop the relay and drop all live conns.
func NewShapedLink(target string, cfg ShapedLinkConfig) (*ShapedLink, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &ShapedLink{
		rateMbps:   cfg.RateMbps,
		rtt:        cfg.RTT,
		queueBytes: cfg.QueueBytes,
		ln:         ln,
		target:     target,
		conns:      map[*connRec]struct{}{},
		doneCh:     make(chan struct{}),
	}
	if cfg.SharedRate {
		s.shared[0] = &fairClock{active: map[*connRec]struct{}{}}
		s.shared[1] = &fairClock{active: map[*connRec]struct{}{}}
	}
	s.wg.Add(1)
	go s.acceptLoop()
	return s, nil
}

// Addr returns the relay's listen address (host:port).
func (s *ShapedLink) Addr() string { return s.ln.Addr().String() }

// URL returns the relay's address as an http base URL, suitable for an agent's
// ServerURL (the agent upgrades http->ws internally).
func (s *ShapedLink) URL() string { return "http://" + s.Addr() }

func (s *ShapedLink) acceptLoop() {
	defer s.wg.Done()
	for {
		client, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			_ = client.Close()
			return
		}
		s.wg.Add(1)
		go s.handle(client)
	}
}

func (s *ShapedLink) handle(client net.Conn) {
	defer s.wg.Done()
	upstream, err := net.Dial("tcp", s.target)
	if err != nil {
		_ = client.Close()
		return
	}
	// The lane tag is derived from the client's first upstream chunk inside the
	// up pump (the agent's WebSocket upgrade request line is plaintext HTTP sent
	// before anything else), so neither direction ever waits on a peek.
	rec := &connRec{
		client:   client,
		upstream: upstream,
		meters:   [2]*meter{newMeter(s.queueBytes), newMeter(s.queueBytes)},
	}
	if !s.track(rec) {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	oneWay := s.rtt / 2
	var bytesPerSec float64
	if s.rateMbps > 0 {
		bytesPerSec = s.rateMbps * 1e6 / 8
	}

	var dwg sync.WaitGroup
	dwg.Add(2)
	go func() { defer dwg.Done(); s.pump(upstream, client, bytesPerSec, oneWay, rec, 0) }()
	go func() { defer dwg.Done(); s.pump(client, upstream, bytesPerSec, oneWay, rec, 1) }()
	dwg.Wait()

	s.untrack(rec)
	_ = client.Close()
	_ = upstream.Close()
}

// tagLane classifies a connection's lane from the client's first upstream chunk
// (the request line carries ?lane=ctrl or ?lane=data); untagged otherwise.
func (s *ShapedLink) tagLane(rec *connRec, head []byte) {
	if i := bytes.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	lane := ""
	switch {
	case bytes.Contains(head, []byte("lane=data")):
		lane = "data"
	case bytes.Contains(head, []byte("lane=ctrl")):
		lane = "ctrl"
	}
	s.mu.Lock()
	rec.lane = lane
	s.mu.Unlock()
}

// scheduled is one chunk in the delay line, tagged with the time it should be
// delivered to dst.
type scheduled struct {
	buf      []byte
	arriveAt time.Time
}

// pump copies one direction src->dst through the rate limiter, delay line, and
// bounded queue. The reader half computes each chunk's transmit schedule (so
// serialization time is consistent within a direction); the writer half sleeps
// until the scheduled delivery time and writes, releasing queue credit only on
// delivery. Reader EOF drains the delay line and then half-closes dst so the
// peer observes EOF. dir selects the meter/clock: 0 = up, 1 = down.
func (s *ShapedLink) pump(dst, src net.Conn, bytesPerSec float64, oneWay time.Duration, rec *connRec, dir int) {
	m := rec.meters[dir]
	clock := s.shared[dir]
	sched := make(chan scheduled, 1024)
	var wwg sync.WaitGroup
	wwg.Add(1)
	go func() {
		defer wwg.Done()
		for item := range sched {
			if d := time.Until(item.arriveAt); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-t.C:
				case <-s.doneCh:
					t.Stop()
				}
			}
			_, err := dst.Write(item.buf)
			m.release(len(item.buf))
			if err != nil {
				// dst gone: keep draining to release credit so the reader is
				// never wedged, but stop writing.
				for extra := range sched {
					m.release(len(extra.buf))
				}
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	var nextFree time.Time
	tagged := dir != 0 // only the up direction carries the client's request line
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !tagged {
				tagged = true
				s.tagLane(rec, buf[:n])
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			if !m.acquire(n) {
				break // link closed
			}
			now := time.Now()
			finish := now
			if bytesPerSec > 0 {
				if clock != nil {
					finish = clock.reserve(rec, dir, now, n, bytesPerSec)
				} else {
					start := now
					if nextFree.After(start) {
						start = nextFree
					}
					finish = start.Add(time.Duration(float64(n) / bytesPerSec * float64(time.Second)))
					nextFree = finish
				}
			}
			select {
			case sched <- scheduled{buf: chunk, arriveAt: finish.Add(oneWay)}:
			case <-s.doneCh:
				m.release(n)
			}
		}
		if err != nil {
			break
		}
	}
	close(sched)
	wwg.Wait()
	if clock != nil {
		clock.forget(rec)
	}
	// Propagate EOF on this direction without killing the reverse direction.
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// fairClock schedules serialization for one direction of a SharedRate link. Each
// connection keeps its own transmit clock; the rate it is clocked at is the
// link capacity divided by the number of connections currently backlogged (their
// clock is ahead of now) in this direction. A connection with no backlog is
// therefore served immediately at its fair share, which is how separate TCP
// flows behave through one bottleneck and what lets the ctrl lane stay fast
// while the data lane saturates the link.
type fairClock struct {
	mu     sync.Mutex
	active map[*connRec]struct{}
}

func (c *fairClock) reserve(rec *connRec, dir int, now time.Time, n int, bytesPerSec float64) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active[rec] = struct{}{}
	backlogged := 0
	for r := range c.active {
		if r.next[dir].After(now) {
			backlogged++
		}
	}
	if !rec.next[dir].After(now) {
		backlogged++ // this connection is about to become backlogged
	}
	share := bytesPerSec / float64(backlogged)
	start := now
	if rec.next[dir].After(start) {
		start = rec.next[dir]
	}
	finish := start.Add(time.Duration(float64(n) / share * float64(time.Second)))
	rec.next[dir] = finish
	return finish
}

func (c *fairClock) forget(rec *connRec) {
	c.mu.Lock()
	delete(c.active, rec)
	c.mu.Unlock()
}

func (s *ShapedLink) track(rec *connRec) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[rec] = struct{}{}
	return true
}

func (s *ShapedLink) untrack(rec *connRec) {
	s.mu.Lock()
	delete(s.conns, rec)
	s.mu.Unlock()
}

// countLane reports how many live relayed connections carry the given lane tag.
func (s *ShapedLink) countLane(lane string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for rec := range s.conns {
		if rec.lane == lane {
			n++
		}
	}
	return n
}

// Drop closes every live connection but keeps the listener open, so a client
// that reconnects gets a fresh shaped conn. Used to model a mid-session link
// cut without tearing down the fixture.
func (s *ShapedLink) Drop() { s.DropWhere(func(string) bool { return true }) }

// DropLane closes only the live connections tagged with lane ("ctrl", "data",
// or "" for untagged), leaving the listener and other lanes untouched. It
// returns how many connections it closed.
func (s *ShapedLink) DropLane(lane string) int {
	return s.DropWhere(func(l string) bool { return l == lane })
}

// DropWhere closes every live connection whose lane tag satisfies pred and
// returns the count. Closing a victim's meters releases any pump parked on a
// full queue so the closed socket is observed promptly.
func (s *ShapedLink) DropWhere(pred func(lane string) bool) int {
	s.mu.Lock()
	var victims []*connRec
	for rec := range s.conns {
		if pred(rec.lane) {
			victims = append(victims, rec)
		}
	}
	s.mu.Unlock()
	for _, rec := range victims {
		_ = rec.client.Close()
		_ = rec.upstream.Close()
		rec.meters[0].closeMeter()
		rec.meters[1].closeMeter()
	}
	return len(victims)
}

// Close stops the listener, drops all live conns, and unblocks any writer that
// is parked on a full queue. It waits for the relay goroutines to exit.
func (s *ShapedLink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.doneCh)
	victims := make([]*connRec, 0, len(s.conns))
	for rec := range s.conns {
		victims = append(victims, rec)
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	for _, rec := range victims {
		_ = rec.client.Close()
		_ = rec.upstream.Close()
		rec.meters[0].closeMeter()
		rec.meters[1].closeMeter()
	}
	s.wg.Wait()
	return nil
}

// meter bounds the number of in-flight (accepted-but-not-yet-delivered) bytes
// in one direction. acquire blocks the reader when the queue is full, which is
// how the fixture reproduces a writer blocking on a deep, full router queue.
type meter struct {
	mu       sync.Mutex
	cond     *sync.Cond
	inflight int
	limit    int // 0 = unlimited
	closed   bool
}

func newMeter(limit int) *meter {
	m := &meter{limit: limit}
	m.cond = sync.NewCond(&m.mu)
	return m
}

// acquire reserves n bytes of queue credit, blocking while the queue is full.
// A chunk larger than the whole queue is admitted only when the queue is empty,
// so it can never deadlock. Returns false once the meter is closed.
func (m *meter) acquire(n int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.limit > 0 && m.inflight > 0 && m.inflight+n > m.limit && !m.closed {
		m.cond.Wait()
	}
	if m.closed {
		return false
	}
	m.inflight += n
	return true
}

func (m *meter) release(n int) {
	m.mu.Lock()
	m.inflight -= n
	m.cond.Broadcast()
	m.mu.Unlock()
}

func (m *meter) closeMeter() {
	m.mu.Lock()
	m.closed = true
	m.cond.Broadcast()
	m.mu.Unlock()
}

var _ io.Closer = (*ShapedLink)(nil)
