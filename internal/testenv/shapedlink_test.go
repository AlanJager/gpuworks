package testenv

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// echoServer accepts one connection and copies it back verbatim. sink, when
// true, drains without echoing. Returns the listen address and a stop func.
func startTCPServer(t *testing.T, handle func(net.Conn)) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); handle(c) }()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close(); wg.Wait() }
}

// TestShapedLink_ThroughputMatchesRate proves the serialization limiter shapes
// bandwidth: a 100 Mbps link delivers a multi-MB stream at a measured rate
// within a tolerance band around 100 Mbps. Throughput is measured at the
// receiver (delivered bytes over wall time), since the sender's Write returns
// as soon as bytes are buffered, not when they cross the shaped link.
func TestShapedLink_ThroughputMatchesRate(t *testing.T) {
	const payload = 4 << 20 // 4 MiB -> ~0.34 s at 100 Mbps
	done := make(chan time.Duration, 1)
	var startCh = make(chan time.Time, 1)
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		n, _ := io.Copy(io.Discard, c)
		start := <-startCh
		if n == payload {
			done <- time.Since(start)
		} else {
			done <- 0
		}
	})
	defer stop()

	link, err := NewShapedLink(target, ShapedLinkConfig{RateMbps: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	conn, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	data := make([]byte, payload)
	start := time.Now()
	startCh <- start
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	select {
	case elapsed := <-done:
		if elapsed == 0 {
			t.Fatal("receiver got wrong byte count")
		}
		mbps := float64(payload*8) / 1e6 / elapsed.Seconds()
		if mbps < 70 || mbps > 135 {
			t.Fatalf("measured throughput %.1f Mbps, want ~100 (70-135 band)", mbps)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not drain payload in time")
	}
}

// TestShapedLink_RTTMatchesConfig proves the delay line adds RTT: a small
// request/echo round-trip takes approximately the configured RTT.
func TestShapedLink_RTTMatchesConfig(t *testing.T) {
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, 16)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				_, _ = c.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	})
	defer stop()

	const rtt = 60 * time.Millisecond
	link, err := NewShapedLink(target, ShapedLinkConfig{RTT: rtt})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	conn, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Warm the relay (dial + accept) with one exchange, then measure.
	ping := []byte("x")
	buf := make([]byte, 1)
	if _, err := conn.Write(ping); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}

	var total, best time.Duration
	const rounds = 5
	for i := 0; i < rounds; i++ {
		start := time.Now()
		if _, err := conn.Write(ping); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatal(err)
		}
		elapsed := time.Since(start)
		total += elapsed
		if best == 0 || elapsed < best {
			best = elapsed
		}
	}
	avg := total / rounds
	// One round trip crosses two one-way delays (RTT/2 each) = ~RTT. The lower
	// bound must hold on average: the shaper adds its delay to every exchange.
	// The upper bound is judged on the best round only: it guards against the
	// shaper inflating the path, and a single relay goroutine descheduled for
	// hundreds of milliseconds on a loaded CI runner inflates one round without
	// saying anything about the shaper.
	if avg < rtt-15*time.Millisecond || best > rtt+40*time.Millisecond {
		t.Fatalf("measured RTT avg %s (best %s), want ~%s", avg, best, rtt)
	}
}

// TestShapedLink_BoundedQueueBlocksWriter proves backpressure reaches the real
// writer: on a rate-limited link with a bounded queue and a sink that never
// drains, a writer pushing far more than any kernel buffer could hold must
// block, and Close must unblock it. The rate limit guarantees the sink's
// autotuned socket buffer cannot mask the block (50 MiB at 10 Mbps would take
// ~40 s to drain even if fully buffered), so a writer still parked after 500 ms
// is genuinely backpressured, not merely slow.
func TestShapedLink_BoundedQueueBlocksWriter(t *testing.T) {
	block := make(chan struct{})
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		<-block // never read until released
	})
	defer stop()
	defer close(block)

	link, err := NewShapedLink(target, ShapedLinkConfig{RateMbps: 10, QueueBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	done := make(chan error, 1)
	go func() {
		data := make([]byte, 50<<20) // 50 MiB, far beyond any socket buffer
		_, err := conn.Write(data)
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("writer completed 50 MiB into a stalled bounded link, want it blocked")
	case <-time.After(500 * time.Millisecond):
		// Still blocked: backpressure reached the writer.
	}

	// Closing the link must unblock the parked writer.
	_ = link.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writer returned nil error after link close, want a failure")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not unblock after link close")
	}
}

// TestShapedLink_UnlimitedPassesBytesVerbatim proves the zero-config relay is a
// faithful byte pipe: content and length are preserved end to end.
func TestShapedLink_UnlimitedPassesBytesVerbatim(t *testing.T) {
	received := make(chan []byte, 1)
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		b, _ := io.ReadAll(c)
		received <- b
	})
	defer stop()

	link, err := NewShapedLink(target, ShapedLinkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	conn, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}

	const payload = 1 << 20
	data := make([]byte, payload)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}

	select {
	case got := <-received:
		if len(got) != payload {
			t.Fatalf("received %d bytes, want %d", len(got), payload)
		}
		if sha256.Sum256(got) != want {
			t.Fatal("payload corrupted through unlimited shaped link")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not receive payload through shaped link")
	}
	_ = conn.Close()
}

// TestShapedLink_CloseUnblocksBothSides proves Close releases readers parked on
// a full queue in either direction and wakes goroutines waiting on the meter.
func TestShapedLink_MeterAdmitsOversizedChunkWhenEmpty(t *testing.T) {
	m := newMeter(1024)
	if !m.acquire(4096) {
		t.Fatal("meter refused an oversized chunk on an empty queue")
	}
	// A second oversized chunk must block until the first is released.
	blocked := make(chan bool, 1)
	go func() { blocked <- m.acquire(4096) }()
	select {
	case <-blocked:
		t.Fatal("meter admitted a second chunk while full")
	case <-time.After(100 * time.Millisecond):
	}
	m.release(4096)
	select {
	case ok := <-blocked:
		if !ok {
			t.Fatal("meter returned false after credit freed")
		}
	case <-time.After(time.Second):
		t.Fatal("meter did not admit after release")
	}
}

func TestShapedLink_MeterCloseUnblocks(t *testing.T) {
	m := newMeter(1024)
	if !m.acquire(1024) {
		t.Fatal("initial acquire failed")
	}
	got := make(chan bool, 1)
	go func() { got <- m.acquire(1024) }()
	select {
	case <-got:
		t.Fatal("acquire returned while queue full")
	case <-time.After(100 * time.Millisecond):
	}
	m.closeMeter()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("acquire returned true after close, want false")
		}
	case <-time.After(time.Second):
		t.Fatal("closeMeter did not unblock a parked acquire")
	}
}

// Guard against an accidental data corruption regression under rate+delay+queue
// all engaged at once.
func TestShapedLink_IntegrityUnderFullShaping(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	done := make(chan struct{})
	target, stop := startTCPServer(t, func(c net.Conn) {
		defer c.Close()
		mu.Lock()
		_, _ = io.Copy(&buf, c)
		mu.Unlock()
		close(done)
	})
	defer stop()

	link, err := NewShapedLink(target, ShapedLinkConfig{RateMbps: 50, RTT: 20 * time.Millisecond, QueueBytes: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()

	conn, err := net.Dial("tcp", link.Addr())
	if err != nil {
		t.Fatal(err)
	}
	const payload = 512 << 10
	data := make([]byte, payload)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(data)
	if _, err := conn.Write(data); err != nil {
		t.Fatal(err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("payload not fully received under full shaping")
	}
	mu.Lock()
	got := buf.Bytes()
	mu.Unlock()
	if len(got) != payload || sha256.Sum256(got) != want {
		t.Fatalf("payload corrupted: got %d bytes", len(got))
	}
	_ = conn.Close()
}
