package hub

import (
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/libp2p/go-yamux/v5"
)

// laneStream is a stream opened on a node's data lane. A transport failure on
// it that the lane's own death caused is reported as ErrDataLaneUnavailable,
// the same retryable condition openStream reports when the lane is gone before
// the stream opens (ADR 0024). Without this a request whose stream was already
// open when the lane dropped surfaces a raw yamux reset, which the proxy can
// only read as a dead upstream (502) although the engine never failed.
//
// The classification is exact, not a guess from timing: go-yamux marks the
// session shut down (and closes its shutdown channel) before it force-closes
// the session's streams with "stream reset: connection closed", and a send
// that fails on a shut-down session returns the shutdown error after the same
// flag is set (Session.close / Session.send). So a stream error observed while
// IsClosed() is true was caused by the session dying. A stream the agent
// resets on its own (its dial to the engine failed, the engine closed the
// connection) leaves the session alive, IsClosed() reports false, and that
// error passes through unchanged as the upstream failure it is.
type laneStream struct {
	net.Conn
	sess  *Session
	ysess *yamux.Session
}

func (c *laneStream) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, c.classify(err)
}

func (c *laneStream) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, c.classify(err)
}

// CloseWrite half-closes the stream as the underlying yamux stream does, so
// wrapping a stream never takes the capability away from a caller that
// type-asserts for it.
func (c *laneStream) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.New("data lane stream does not support CloseWrite")
}

// Reset aborts the stream in both directions as the underlying yamux stream
// does; Close only half-closes it, which the agent reads as a clean end.
func (c *laneStream) Reset() error {
	if r, ok := c.Conn.(interface{ Reset() error }); ok {
		return r.Reset()
	}
	return c.Conn.Close()
}

func (c *laneStream) classify(err error) error {
	// A clean end is io.EOF itself, which Readers return unwrapped. It is
	// compared, not matched with errors.Is: the reset a dying session hands
	// its streams wraps the session's own shutdown cause, which is io.EOF
	// when the agent's side of the lane simply went away.
	if err == nil || err == io.EOF || !c.ysess.IsClosed() {
		return err
	}
	// The session is already shut down; detaching it now wakes waiters and
	// keeps routing from considering it, rather than waiting for the
	// CloseChan watcher to catch up.
	c.sess.detachDataIfCurrent(c.ysess)
	return fmt.Errorf("%w: node %s data lane closed under an open stream: %w", ErrDataLaneUnavailable, c.sess.NodeUUID, err)
}
