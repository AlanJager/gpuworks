// Package wsconn adapts a WebSocket to the net.Conn that a yamux lane is
// layered on. It exists because the adapter's write deadline, left as the
// library implements it, drops live agent sessions on a healthy link.
//
// coder/websocket's net.Conn implements the write deadline with a timer whose
// callback sets a sticky "expired" flag; every later Write is rejected, without
// touching the socket, until SetWriteDeadline clears the flag again. Timer
// callbacks run on their own goroutine, and rescheduling a timer does not
// recall a callback that has already been released. go-yamux arms that deadline
// from its send loop and then refreshes it only once it is past the halfway
// point, so a callback released at the deadline and delayed past the next
// refresh sets the flag after the refresh cleared it, and the next frame -
// which is inside the halfway window and therefore does not re-arm anything -
// is failed. go-yamux treats any send-loop error as fatal, so one stale
// callback closes the whole session: on the server that deregisters the node,
// which is indistinguishable from a real disconnect.
//
// Wrap tells the difference between that and a real stall the only way that is
// sound: a deadline that has not yet passed cannot have expired, so a rejection
// claiming it has is stale. The deadline is re-armed and the frame written
// once. A write that genuinely exceeds its deadline, or that fails for any
// other reason, is returned untouched.
//
// This covers only a callback that finds no write in flight. The same callback,
// finding one, cancels the adapter's write context instead, and that context
// is never replaced: coder/websocket closes the whole connection when a write's
// context is cancelled, and every later write would fail anyway. Nothing here
// can undo that, so the lanes keep the keepalive interval well short of half
// the write timeout (see internal/server/hub/yamux.go) so that no keepalive
// frame is ever written at the deadline instant in the first place.
package wsconn

import (
	"context"
	"errors"
	"net"
	"time"
)

// errNoHalfClose is what CloseWrite reports for a connection that has no half
// close of its own; callers treat it the way they treat a connection that never
// offered one.
var errNoHalfClose = errors.New("connection does not support a half close")

// Wrap returns conn with a write path that does not fail on a stale deadline
// expiry. Both lanes of both ends use it; nothing else about the connection
// changes.
func Wrap(conn net.Conn) net.Conn { return &deadlineSafeConn{Conn: conn} }

// deadlineSafeConn remembers the write deadline yamux last asked for. Both
// SetWriteDeadline and Write are called only from the yamux send loop, which is
// a single goroutine, so the field needs no lock; the flag being corrected is
// set on a timer goroutine inside the wrapped connection.
type deadlineSafeConn struct {
	net.Conn
	writeDeadline time.Time
}

func (c *deadlineSafeConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.Conn.SetWriteDeadline(t)
}

func (c *deadlineSafeConn) SetDeadline(t time.Time) error {
	c.writeDeadline = t
	return c.Conn.SetDeadline(t)
}

// CloseWrite forwards the half-close the lane teardown path looks for, so
// wrapping a connection never costs it a capability it had. A connection that
// cannot half-close says so, exactly as an unwrapped one would.
func (c *deadlineSafeConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return errNoHalfClose
}

func (c *deadlineSafeConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if !c.staleExpiry(n, err) {
		return n, err
	}
	// Re-arming clears the flag the stale callback set. The frame was never put
	// on the wire (the rejection happens before the socket is touched), so
	// writing it now cannot duplicate bytes.
	if reArmErr := c.Conn.SetWriteDeadline(c.writeDeadline); reArmErr != nil {
		return n, err
	}
	return c.Conn.Write(p)
}

// staleExpiry reports whether err is a write-deadline rejection for a deadline
// that has not actually passed. Anything else - a real timeout, a cancelled
// connection, a closed socket, a partial write - is not this bug and must reach
// yamux unchanged.
func (c *deadlineSafeConn) staleExpiry(n int, err error) bool {
	if err == nil || n != 0 {
		return false
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return !c.writeDeadline.IsZero() && time.Now().Before(c.writeDeadline)
}
