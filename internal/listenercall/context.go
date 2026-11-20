// Package listenercall carries one request's exact listener cleanup ownership.
package listenercall

import (
	"context"
	"errors"
	"sync"
)

type contextKey struct{}

type Call struct {
	parent  context.Context
	mu      sync.Mutex
	cleanup func(context.Context) error
}

// New retains the original request parent independently of the context used
// to bound waiting for its start response.
func New(parent context.Context) (context.Context, *Call) {
	c := &Call{parent: parent}
	return context.WithValue(parent, contextKey{}, c), c
}

func From(ctx context.Context) *Call {
	c, _ := ctx.Value(contextKey{}).(*Call)
	return c
}

func (c *Call) Parent() context.Context { return c.parent }

func (c *Call) SetCleanup(cleanup func(context.Context) error) error {
	if cleanup == nil {
		return errors.New("listener cleanup callback is required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cleanup != nil {
		return errors.New("listener cleanup callback was already published")
	}
	c.cleanup = cleanup
	return nil
}

// Cleanup keeps the published callback for exact repeated cleanup. The
// callback owns its stop/join protocol and receives the supplied wait context.
func (c *Call) Cleanup(ctx context.Context) (bool, error) {
	c.mu.Lock()
	cleanup := c.cleanup
	c.mu.Unlock()
	if cleanup == nil {
		return false, nil
	}
	return true, cleanup(ctx)
}
