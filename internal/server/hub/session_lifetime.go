package hub

import (
	"context"
	"errors"
	"sync"
)

var errSessionsStopped = errors.New("agent sessions are stopped")

// sessionLifetime extends the existing work counter with an admission gate and
// cancellation. A node session exists only after authentication, so its table
// cannot account for a pending ctrl/data handshake or outbound Dial.
type sessionLifetime struct {
	sync.WaitGroup
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	stopped chan struct{}
}

func (l *sessionLifetime) begin() (context.Context, func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx == nil {
		l.ctx, l.cancel = context.WithCancel(context.Background())
	}
	if l.ctx.Err() != nil {
		return nil, nil, false
	}
	l.Add(1)
	return l.ctx, l.Done, true
}

func (l *sessionLifetime) start() {
	l.mu.Lock()
	done := l.stopped
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped == done && (l.ctx == nil || l.ctx.Err() != nil) {
		l.ctx, l.cancel = context.WithCancel(context.Background())
		l.stopped = nil
	}
}

func (l *sessionLifetime) stop() {
	l.mu.Lock()
	if done := l.stopped; done != nil {
		l.mu.Unlock()
		<-done
		return
	}
	l.stopped = make(chan struct{})
	done := l.stopped
	if l.ctx == nil {
		l.ctx, l.cancel = context.WithCancel(context.Background())
	}
	l.cancel()
	l.mu.Unlock()
	l.Wait()
	close(done)
}

// StartSessions reopens session admission after the previous StopSessions has
// drained. The App calls it before serving a new Run.
func (h *Hub) StartSessions() { h.sessionWork.start() }

// StopSessions refuses new connections, cancels even unauthenticated
// handshakes, and waits for all their store/ingest work before the App closes
// its stores. Parent connections register before doing work; their children
// register synchronously while a parent is still counted.
func (h *Hub) StopSessions() { h.sessionWork.stop() }
