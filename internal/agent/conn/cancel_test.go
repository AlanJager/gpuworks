package conn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// queuedCancelContext models a parent whose Done already closed while its
// scheduled AfterFunc has not yet propagated cancellation to a child. Holding
// that callback makes the real cancellation/read-error race deterministic.
type queuedCancelContext struct {
	context.Context
	done     chan struct{}
	linked   chan struct{}
	mu       sync.Mutex
	callback func()
	stopped  bool
}

func (c *queuedCancelContext) Done() <-chan struct{} { return c.done }
func (c *queuedCancelContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}
func (c *queuedCancelContext) AfterFunc(callback func()) func() bool {
	c.mu.Lock()
	c.callback = callback
	c.mu.Unlock()
	close(c.linked)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.stopped {
			return false
		}
		c.stopped = true
		return true
	}
}
func (c *queuedCancelContext) release() {
	c.mu.Lock()
	callback := c.callback
	stopped := c.stopped
	c.mu.Unlock()
	if callback != nil && !stopped {
		callback()
	}
}

type shutdownReadError struct {
	gate <-chan struct{}
	err  error
}

func (r shutdownReadError) Read([]byte) (int, error) { <-r.gate; return 0, r.err }

func TestServeParentCancellationWinsBeforeChildPropagation(t *testing.T) {
	parent := &queuedCancelContext{Context: context.Background(), done: make(chan struct{}), linked: make(chan struct{})}
	defer parent.release()
	gate := make(chan struct{})
	transportError := errors.New("transport closed during shutdown")
	c := &Conn{dec: json.NewDecoder(shutdownReadError{gate, transportError})}
	result := make(chan error, 1)
	go func() { result <- c.serve(parent) }()
	select {
	case <-parent.linked:
	case <-time.After(time.Second):
		t.Fatal("serve child cancellation was not linked")
	}
	close(parent.done) // Parent canceled, child callback deliberately queued.
	close(gate)        // The real transport may notice the shutdown first.
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("caller cancellation reported a lost connection: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve did not finish")
	}
}

func TestServeLiveParentPreservesTransportError(t *testing.T) {
	gate := make(chan struct{})
	close(gate)
	c := &Conn{dec: json.NewDecoder(shutdownReadError{gate, io.ErrUnexpectedEOF})}
	if err := c.serve(context.Background()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("live connection error was hidden: %v", err)
	}
}
