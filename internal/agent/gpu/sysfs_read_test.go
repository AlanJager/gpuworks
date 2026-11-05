package gpu

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBoundedSysfsReadKeepsSlotUntilCanceledReadActuallyReturns(t *testing.T) {
	var gate sysfsReadGate
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := boundedSysfsRead(ctx, &gate, func() (int, error) { calls.Add(1); close(started); <-release; return 7, nil })
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	for i := 0; i < 3; i++ {
		retryCtx, stop := context.WithTimeout(context.Background(), time.Millisecond)
		_, err := boundedSysfsRead(retryCtx, &gate, func() (int, error) { calls.Add(1); return 8, nil })
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("retry error=%v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d blocked readers", calls.Load())
	}
	close(release)
	nextCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	value, err := boundedSysfsRead(nextCtx, &gate, func() (int, error) { return 9, nil })
	if err != nil || value != 9 {
		t.Fatalf("slot not released: %d %v", value, err)
	}
}

func TestBoundedSysfsReadCanceledCallerDoesNotStartIO(t *testing.T) {
	var gate sysfsReadGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := boundedSysfsRead(ctx, &gate, func() (int, error) { t.Error("canceled caller started I/O"); return 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}
