package cardhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// A hundred people asking about the same cell make one call; for five seconds
// after, they get its answer without a call.
func TestDetailsAskOnceAndReuseTheAnswer(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	d := NewDetails(func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
		calls.Add(1)
		if method != proto.RPCNodeCardCheckDetail {
			t.Errorf("method %q", method)
		}
		<-release
		req := params.(proto.CardCheckDetailRequest)
		return json.Marshal(proto.CardCheckDetail{Ident: req.Ident, Check: req.Check})
	})
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	d.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }

	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := d.Detail(context.Background(), "n1", "uuid:GPU-1", "runtime")
			if err == nil && got.Check != "runtime" {
				err = fmt.Errorf("got %+v", got)
			}
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want one", calls.Load())
	}
	if _, err := d.Detail(context.Background(), "n1", "uuid:GPU-1", "runtime"); err != nil || calls.Load() != 1 {
		t.Fatalf("a cached answer called again (%d), %v", calls.Load(), err)
	}
	mu.Lock()
	now = now.Add(detailCacheFor)
	mu.Unlock()
	if _, err := d.Detail(context.Background(), "n1", "uuid:GPU-1", "runtime"); err != nil || calls.Load() != 2 {
		t.Fatalf("an old answer was reused (%d calls), %v", calls.Load(), err)
	}
}

// At most two requests per node and sixteen in all are in flight; the rest
// are told to come back.
func TestDetailsBoundTheRequestsInFlight(t *testing.T) {
	release := make(chan struct{})
	d := NewDetails(func(context.Context, string, string, any) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	})
	defer close(release)
	ask := func(node, check string) chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := d.Detail(context.Background(), node, "uuid:GPU-1", check)
			ch <- err
		}()
		return ch
	}
	waitInFlight := func(n int) {
		for i := 0; i < 200; i++ {
			d.mu.Lock()
			total := d.total
			d.mu.Unlock()
			if total == n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("in flight never reached %d", n)
	}
	ask("n1", "runtime")
	ask("n1", "xid")
	waitInFlight(2)
	if _, err := d.Detail(context.Background(), "n1", "uuid:GPU-1", "nvlink"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third on one node = %v, want busy", err)
	}
	for i := 0; i < 14; i++ {
		ask(fmt.Sprintf("node-%d", i), "runtime")
	}
	waitInFlight(16)
	if _, err := d.Detail(context.Background(), "another", "uuid:GPU-1", "runtime"); !errors.Is(err, ErrBusy) {
		t.Fatalf("seventeenth = %v, want busy", err)
	}
}

// A failed call is not kept: the next ask calls again.
func TestDetailsDoNotKeepFailures(t *testing.T) {
	var calls atomic.Int32
	d := NewDetails(func(context.Context, string, string, any) (json.RawMessage, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("rpc timeout")
		}
		return json.RawMessage(`{"check":"runtime"}`), nil
	})
	if _, err := d.Detail(context.Background(), "n1", "i", "runtime"); err == nil {
		t.Fatal("the failure was not returned")
	}
	if got, err := d.Detail(context.Background(), "n1", "i", "runtime"); err != nil || got.Check != "runtime" || calls.Load() != 2 {
		t.Fatalf("retry = %+v %v (%d calls)", got, err, calls.Load())
	}
}

// A caller that gives up gets its context's error; the call goes on for the
// others.
func TestDetailsHonourTheCallersContext(t *testing.T) {
	release := make(chan struct{})
	d := NewDetails(func(context.Context, string, string, any) (json.RawMessage, error) {
		<-release
		return json.RawMessage(`{}`), nil
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Detail(ctx, "n1", "i", "runtime"); !errors.Is(err, context.Canceled) {
		t.Fatalf("= %v", err)
	}
}
