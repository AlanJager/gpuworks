package wsconn

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recordingConn is a net.Conn stand-in whose Write result is scripted, so the
// wrapper's one decision - is this rejection stale, or real - can be put under
// each case it has to tell apart.
type recordingConn struct {
	net.Conn
	results   []writeResult
	writes    int
	deadlines []time.Time
	setErr    error
}

type writeResult struct {
	n   int
	err error
}

func (c *recordingConn) Write(p []byte) (int, error) {
	index := c.writes
	c.writes++
	if index < len(c.results) {
		return c.results[index].n, c.results[index].err
	}
	return len(p), nil
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.setErr
}

func (c *recordingConn) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.setErr
}

// The rejection the adapter produces when a stale timer callback sets its
// expiry flag: the write never reached the socket, so n is zero.
func staleRejection() writeResult {
	return writeResult{n: 0, err: fmt.Errorf("failed to write: %w", context.DeadlineExceeded)}
}

func TestWriteRetriesOnceWhenTheDeadlineHasNotActuallyPassed(t *testing.T) {
	inner := &recordingConn{results: []writeResult{staleRejection()}}
	conn := Wrap(inner)
	deadline := time.Now().Add(time.Minute)
	if err := conn.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	n, err := conn.Write([]byte("frame"))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len("frame") {
		t.Fatalf("wrote %d bytes, want %d", n, len("frame"))
	}
	if inner.writes != 2 {
		t.Fatalf("underlying writes = %d, want 2 (the rejected one and the retry)", inner.writes)
	}
	// The retry has to re-arm the deadline: that is what clears the flag the
	// stale callback set. Re-arming with a different deadline would silently
	// shorten or extend what yamux asked for.
	if len(inner.deadlines) != 2 || !inner.deadlines[1].Equal(deadline) {
		t.Fatalf("deadlines armed = %v, want the same deadline armed twice", inner.deadlines)
	}
}

func TestWriteReturnsATimeoutWhoseDeadlineHasPassed(t *testing.T) {
	inner := &recordingConn{results: []writeResult{staleRejection()}}
	conn := Wrap(inner)
	if err := conn.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	if _, err := conn.Write([]byte("frame")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v, want the deadline error passed through", err)
	}
	if inner.writes != 1 {
		t.Fatalf("underlying writes = %d, want 1: an expired deadline must not be retried", inner.writes)
	}
}

func TestWriteDoesNotRetryOtherFailures(t *testing.T) {
	cases := map[string]writeResult{
		"a cancelled connection": {n: 0, err: fmt.Errorf("failed to write msg: %w", context.Canceled)},
		"a closed socket":        {n: 0, err: net.ErrClosed},
		"a partial write":        {n: 3, err: fmt.Errorf("failed to write: %w", context.DeadlineExceeded)},
	}
	for name, result := range cases {
		t.Run(name, func(t *testing.T) {
			inner := &recordingConn{results: []writeResult{result}}
			conn := Wrap(inner)
			if err := conn.SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
				t.Fatalf("set write deadline: %v", err)
			}
			n, err := conn.Write([]byte("frame"))
			if err == nil {
				t.Fatal("want the underlying error to reach the caller")
			}
			if n != result.n {
				t.Fatalf("n = %d, want %d unchanged", n, result.n)
			}
			if inner.writes != 1 {
				t.Fatalf("underlying writes = %d, want 1", inner.writes)
			}
		})
	}
}

func TestWriteReturnsTheOriginalErrorWhenTheDeadlineCannotBeReArmed(t *testing.T) {
	inner := &recordingConn{results: []writeResult{staleRejection()}, setErr: errors.New("conn is gone")}
	conn := Wrap(inner)
	_ = conn.SetWriteDeadline(time.Now().Add(time.Minute))
	if _, err := conn.Write([]byte("frame")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write error = %v, want the original rejection", err)
	}
	if inner.writes != 1 {
		t.Fatalf("underlying writes = %d, want 1: a connection that cannot be re-armed must not be written to again", inner.writes)
	}
}

func TestSetDeadlineAlsoArmsTheWriteSide(t *testing.T) {
	inner := &recordingConn{results: []writeResult{staleRejection()}}
	conn := Wrap(inner)
	if err := conn.SetDeadline(time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := conn.Write([]byte("frame")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if inner.writes != 2 {
		t.Fatalf("underlying writes = %d, want 2", inner.writes)
	}
}

// TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant drives the real adapter
// the way a yamux send loop drives it - arm the deadline only once past the
// halfway point, then write one frame - against a real WebSocket, with the
// write landing at the deadline instant. That is not a contrived alignment: a
// lane's keepalive interval is exactly half its connection write timeout, so a
// session with nothing else to say writes its ping or pong at that instant
// repeatedly. Unwrapped, connections die there; wrapped, none do.
//
// A wrapped failure is only a regression when the error surfaced while the
// armed deadline was still in the future: a genuine OS timeout cannot fire
// early, while the stale-callback rejection returns instantly. Under load the
// writing goroutine itself can be descheduled past its deadline, and that
// expiry is the adapter working exactly as documented.
func TestLiveWebSocketSurvivesAWriteAtTheDeadlineInstant(t *testing.T) {
	const (
		deadline = 60 * time.Millisecond
		rounds   = 20
		burst    = 20
		conns    = 16
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		nc := websocket.NetConn(context.Background(), c, websocket.MessageBinary)
		buf := make([]byte, 4096)
		for {
			if _, err := nc.Read(buf); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	// Runnable goroutines delay the adapter's timer callback, which is what
	// lets it land after a refresh instead of before it. A fleet of agents
	// supplies this pressure by itself.
	stop := make(chan struct{})
	var load sync.WaitGroup
	for i := 0; i < 4*runtime.GOMAXPROCS(0); i++ {
		load.Add(1)
		go func() {
			defer load.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < 200000; j++ {
					_ = j * j
				}
				runtime.Gosched()
			}
		}()
	}
	defer func() { close(stop); load.Wait() }()

	run := func(wrap bool) (rejected int64, stale int64, stalled int64) {
		var rejectedCount, staleCount, stalledCount atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < conns; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				if err != nil {
					return
				}
				var conn net.Conn = websocket.NetConn(ctx, c, websocket.MessageBinary)
				if wrap {
					conn = Wrap(conn)
				}
				defer conn.Close()

				var armed time.Time
				frame := make([]byte, 12)
				for round := 0; round < rounds; round++ {
					// Idle right up to the deadline, then write: the callback
					// and the write are released together.
					if wait := time.Until(armed); wait > 0 {
						time.Sleep(wait)
					}
					for j := 0; j < burst; j++ {
						now := time.Now()
						if now.Add(deadline / 2).After(armed) {
							armed = now.Add(deadline)
							_ = conn.SetWriteDeadline(armed)
						}
						start := time.Now()
						if _, err := conn.Write(frame); err != nil {
							if errors.Is(err, context.DeadlineExceeded) {
								rejectedCount.Add(1)
								// Only an error that surfaced before the armed
								// deadline can be the stale-callback rejection; one
								// that surfaces after it is a genuine expiry, which
								// load can inflict on any goroutine.
								if time.Now().Before(armed) {
									staleCount.Add(1)
								}
								if time.Since(start) > 5*time.Millisecond {
									stalledCount.Add(1)
								}
							}
							return
						}
						time.Sleep(time.Millisecond)
					}
				}
			}()
		}
		wg.Wait()
		return rejectedCount.Load(), staleCount.Load(), stalledCount.Load()
	}

	bare, bareStale, bareStalled := run(false)
	// The stalled count is reported, not asserted: a rejection that returns
	// before the socket is touched can still be measured as slow if the
	// goroutine is descheduled in between, which says nothing about the bug.
	t.Logf("unwrapped: %d of %d connections failed on a deadline that had just been re-armed (%d of those stale, %d measured longer than 5ms, which is scheduling, not waiting)", bare, conns, bareStale, bareStalled)
	wrapped, wrappedStale, _ := run(true)
	if wrapped > 0 {
		t.Logf("wrapped: %d connection(s) expired on deadlines that had genuinely passed under load; only pre-deadline failures count as the bug", wrapped)
	}
	if wrappedStale != 0 {
		t.Errorf("wrapped: %d connection(s) still failed on a deadline that had not passed", wrappedStale)
	}
	if bare == 0 {
		t.Log("the unwrapped adapter did not fail in this run, so this run only shows the wrapper is harmless")
	}
}

// halfCloser is a connection that can half-close, which the lane teardown path
// looks for with a type assertion. Wrapping must not hide it.
type halfCloser struct {
	recordingConn
	closed bool
}

func (c *halfCloser) CloseWrite() error { c.closed = true; return nil }

func TestCloseWriteReachesAConnectionThatHasOne(t *testing.T) {
	inner := &halfCloser{}
	conn := Wrap(inner)
	closer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the wrapper must offer a half close")
	}
	if err := closer.CloseWrite(); err != nil {
		t.Fatalf("close write: %v", err)
	}
	if !inner.closed {
		t.Fatal("the half close did not reach the underlying connection")
	}
}

func TestCloseWriteReportsAConnectionThatHasNone(t *testing.T) {
	conn := Wrap(&recordingConn{})
	closer, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("the wrapper always offers the method; it is the error that says no")
	}
	if err := closer.CloseWrite(); err == nil {
		t.Fatal("want an error for a connection with no half close")
	}
}
