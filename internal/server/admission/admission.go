// Package admission owns the serialized transaction boundary for resource
// claims. API and controller callers share one Gate instance so a placement
// decision and its claim rows cannot race each other.
package admission

import (
	"context"
	"errors"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Effects are notifications that become visible only after the transaction
// callback returns successfully and the transaction runner commits. The
// callback must not publish events or enqueue work itself.
type Effects struct {
	Events  []bus.Event
	Enqueue []string
}

// Transaction runs fn in one database transaction. It must call fn at most
// once, commit only when fn returns nil, and return the commit error to Gate.
// The context passed to fn is transaction-scoped. A store adapter may attach
// its opaque transaction handle with WithTransactionHandle; admission itself
// never inspects or depends on that handle type.
type Transaction func(context.Context, func(context.Context) (Effects, error)) error

type transactionHandleKey struct{}

// WithTransactionHandle attaches an opaque transaction handle for the
// operation callback. This keeps the admission package independent of GORM or
// any other persistence driver while allowing a real store adapter to expose
// its scoped transaction to the callback.
func WithTransactionHandle(ctx context.Context, handle any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, transactionHandleKey{}, handle)
}

// TransactionHandle retrieves the opaque handle attached by a transaction
// adapter. Callers should type-assert it to their own store transaction type.
func TransactionHandle(ctx context.Context) (any, bool) {
	if ctx == nil {
		return nil, false
	}
	handle := ctx.Value(transactionHandleKey{})
	return handle, handle != nil
}

// EventPublisher receives committed change notifications.
type EventPublisher interface {
	Publish(bus.Event)
}

// Enqueuer receives committed reconcile keys. Implementations should preserve
// the order in which keys are supplied; duplicate coalescing belongs to the
// queue implementation.
type Enqueuer interface {
	Enqueue(string)
}

// Gate is the sole admission boundary for claim-adding operations. Calls are
// serialized across the complete transaction and post-commit effect dispatch,
// which gives callers a stable order for commit, publish, and enqueue.
type Gate struct {
	mu          sync.Mutex
	transaction Transaction
	publisher   EventPublisher
	enqueuer    Enqueuer
}

// New constructs a gate. Transaction is required; publisher and enqueuer may
// be nil for callers that have no corresponding post-commit effect.
func New(transaction Transaction, publisher EventPublisher, enqueuer Enqueuer) (*Gate, error) {
	if transaction == nil {
		return nil, errors.New("admission: nil transaction runner")
	}
	return &Gate{transaction: transaction, publisher: publisher, enqueuer: enqueuer}, nil
}

// Run executes fn in one serialized transaction and dispatches its effects only
// after a successful commit. Effects are discarded on callback or transaction
// failure. fn must not perform network or other external I/O; it is the narrow
// place for database writes and deterministic claim validation.
func (g *Gate) Run(ctx context.Context, fn func(context.Context) (Effects, error)) error {
	if g == nil || g.transaction == nil {
		return errors.New("admission: unconfigured gate")
	}
	if fn == nil {
		return errors.New("admission: nil operation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	var effects Effects
	err := g.transaction(ctx, func(txCtx context.Context) (Effects, error) {
		if txCtx == nil {
			txCtx = ctx
		}
		var callbackErr error
		effects, callbackErr = fn(txCtx)
		if callbackErr != nil {
			effects = Effects{}
		}
		return effects, callbackErr
	})
	if err != nil {
		return err
	}

	for _, event := range effects.Events {
		if g.publisher != nil {
			g.publisher.Publish(event)
		}
	}
	for _, key := range effects.Enqueue {
		if g.enqueuer != nil {
			g.enqueuer.Enqueue(key)
		}
	}
	return nil
}
