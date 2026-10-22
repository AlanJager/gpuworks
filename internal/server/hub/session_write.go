package hub

import (
	"io"
	"time"
)

// ctrlWriteTimeout bounds a peer that stops consuming its yamux stream. The
// connection's own write timeout does not cover an exhausted stream window.
const ctrlWriteTimeout = 30 * time.Second

func (s *Session) writeCtrl(line []byte) error {
	s.writeMu.Lock()
	// Start the budget after earlier writers finish, not while waiting for
	// the store or the full-replay concurrency limit.
	deadline, hasDeadline := s.ctrl.(interface{ SetWriteDeadline(time.Time) error })
	var err error
	if hasDeadline {
		err = deadline.SetWriteDeadline(s.hub.now().Add(ctrlWriteTimeout))
	}
	if err == nil {
		var n int
		n, err = s.ctrl.Write(line)
		if err == nil && n != len(line) {
			err = io.ErrShortWrite
		}
	}
	if hasDeadline {
		clearErr := deadline.SetWriteDeadline(time.Time{})
		if err == nil {
			err = clearErr
		}
	}
	s.writeMu.Unlock()
	if err != nil {
		// Partial JSON lines cannot be resumed. Close this captured session,
		// never a replacement connection with the same node UUID.
		s.Close()
	}
	return err
}
