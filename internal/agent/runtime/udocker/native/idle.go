package native

import (
	"context"
	"io"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
)

// idleReader watches one response body. It aborts the transfer when the
// connection delivers nothing for idleTimeout, or its budget lease judges it
// to be crawling (see slowMinRate), and reports bytes as they arrive so
// progress reflects the network rather than the writer.
//
// Aborting means cancelling the request itself: a Read blocked on a silent
// connection returns only once that context is cancelled, so the cancel passed
// in must be the one belonging to the context the request was made with.
type idleReader struct {
	inner  io.Reader
	parent context.Context
	report func(int64)
	timer  *time.Timer
	cancel context.CancelFunc
	idle   atomic.Bool
	lease  *connbudget.Lease
}

func newIdleReader(parent context.Context, cancel context.CancelFunc, inner io.Reader, lease *connbudget.Lease, report func(int64)) *idleReader {
	r := &idleReader{inner: inner, parent: parent, report: report, cancel: cancel, lease: lease}
	r.timer = time.AfterFunc(idleTimeout, func() {
		r.idle.Store(true)
		cancel()
	})
	return r
}

// stop releases the reader's timer. Reading until an error does the same, but
// a caller that stops short of the end must call it.
func (r *idleReader) stop() { r.timer.Stop() }

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.timer.Reset(idleTimeout)
		if r.report != nil {
			r.report(int64(n))
		}
		if r.lease.Observe(int64(n)) && err == nil {
			err = errSlow
		}
	}
	if r.idle.Load() && r.parent.Err() == nil && err != io.EOF {
		// The timer fired while the transfer was silent, and whatever the
		// cancelled request reports (context canceled, a reset) is the symptom.
		err = errIdle
	}
	if err != nil {
		r.stop()
	}
	return n, err
}
