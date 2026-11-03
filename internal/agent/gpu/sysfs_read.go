package gpu

import (
	"context"
	"sync"
)

// sysfsReadGate owns at most one kernel read. A canceled caller stops waiting,
// but the slot stays occupied until the uninterruptible sysfs read returns.
// This bounds abandoned readers without pretending cancellation stops I/O.
type sysfsReadGate struct {
	once sync.Once
	slot chan struct{}
}

func boundedSysfsRead[T any](ctx context.Context, gate *sysfsReadGate, read func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	gate.once.Do(func() { gate.slot = make(chan struct{}, 1) })
	select {
	case gate.slot <- struct{}{}:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	// Cancellation may have raced the available slot; do not start new I/O.
	if err := ctx.Err(); err != nil {
		<-gate.slot
		return zero, err
	}
	type result struct {
		value T
		err   error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-gate.slot }()
		value, err := read()
		done <- result{value, err}
	}()
	select {
	case r := <-done:
		return r.value, r.err
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}
