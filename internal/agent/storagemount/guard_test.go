package storagemount

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuardReturnsResultAndError(t *testing.T) {
	g := newGuard()
	if err := g.run(context.Background(), "k", time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	if err := g.run(context.Background(), "k", time.Second, func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	// The key is free again right after the result was delivered.
	for i := 0; i < 100; i++ {
		if err := g.run(context.Background(), "k", time.Second, func() error { return nil }); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
}

func TestGuardTimeoutThenSingleFlight(t *testing.T) {
	g := newGuard()
	release := make(chan struct{})
	var started atomic.Int32
	stuck := func() error { started.Add(1); <-release; return nil }

	start := time.Now()
	if err := g.run(context.Background(), "nfs", 50*time.Millisecond, stuck); !errors.Is(err, ErrUnresponsive) {
		t.Fatalf("first call = %v, want ErrUnresponsive", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("timeout took %s", d)
	}

	// While the first call is stuck, new calls fail at once and start no goroutine.
	for i := 0; i < 50; i++ {
		begin := time.Now()
		if err := g.run(context.Background(), "nfs", time.Minute, stuck); !errors.Is(err, ErrUnresponsive) {
			t.Fatalf("call %d = %v", i, err)
		}
		if time.Since(begin) > 200*time.Millisecond {
			t.Fatalf("call %d blocked", i)
		}
	}
	if n := started.Load(); n != 1 {
		t.Fatalf("%d calls started, want exactly 1", n)
	}
	// Other keys are not affected.
	if err := g.run(context.Background(), "other", time.Second, func() error { return nil }); err != nil {
		t.Fatalf("other key: %v", err)
	}

	close(release)
	eventually(t, "key release", func() bool {
		return g.run(context.Background(), "nfs", time.Second, func() error { return nil }) == nil
	})
	if err := g.run(context.Background(), "nfs", time.Second, func() error { return nil }); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

func TestGuardContextCancel(t *testing.T) {
	g := newGuard()
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := g.run(ctx, "k", time.Minute, func() error { <-release; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if err := g.run(context.Background(), "k", time.Second, func() error { return nil }); !errors.Is(err, ErrUnresponsive) {
		t.Errorf("the abandoned call must still hold the key, got %v", err)
	}
}

func TestManagerDoAnyRunsSideBySideUpToSlots(t *testing.T) {
	m := New(Options{Statfs: func(string) (Usage, error) { return Usage{TotalBytes: 10, AvailableBytes: 4}, nil }})
	release := make(chan struct{})
	var started atomic.Int32
	stuck := func() error { started.Add(1); <-release; return nil }
	for i := 0; i < 2; i++ {
		if err := m.DoAny(7, "list", 2, 20*time.Millisecond, stuck); !errors.Is(err, ErrUnresponsive) {
			t.Fatalf("slot %d = %v, want ErrUnresponsive after its timeout", i, err)
		}
	}
	// Both slots are held by stuck calls: the third is refused at once.
	begin := time.Now()
	if err := m.DoAny(7, "list", 2, time.Minute, stuck); !errors.Is(err, ErrUnresponsive) || time.Since(begin) > 200*time.Millisecond {
		t.Fatalf("third call = %v after %s", err, time.Since(begin))
	}
	if n := started.Load(); n != 2 {
		t.Fatalf("%d calls started, want 2", n)
	}
	// Another storage is unaffected; slots < 1 means one.
	if err := m.DoAny(8, "list", 0, time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	eventually(t, "slots free", func() bool {
		return m.DoAny(7, "list", 2, time.Second, func() error { return nil }) == nil
	})
	if u, err := m.Statfs("/x"); err != nil || u.AvailableBytes != 4 {
		t.Fatalf("Statfs = %+v, %v", u, err)
	}
}
