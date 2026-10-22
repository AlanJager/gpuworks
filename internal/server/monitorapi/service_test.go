package monitorapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// slowFirstQueryStore wraps a real tsdb.Store and blocks its first
// QueryRange call until a fresh context.WithTimeout(background, blockFor)
// expires, then behaves normally for every call after - simulating a
// single hung backend call (a stuck disk, a lock some other process never
// released) without ever actually blocking forever (the context always
// carries its own deadline, so the test itself can't hang even if the
// timeout wiring were broken).
//
// blockFor is deliberately an order of magnitude larger than computeTimeout
// (2s vs 100ms - see TestOverviewComputeTimeoutFailsAndDoesNotOutliveNegativeTTL),
// not merely "the same duration" as an earlier version of this fixture used.
// Tying the two to the same duration and relying on blockFor's timer being
// *created* strictly after computeTimeout's deadline was set (so it would,
// in theory, always fire strictly later) turned out not to be enough:
// context.Context's deadline expiry is timer-driven (context.WithDeadline
// uses time.AfterFunc internally), and under real scheduler load the gap
// between the two timers' creation - just the time to construct one tsdb
// query - can be microseconds, well within the jitter a busy machine
// introduces into when each timer's callback actually runs. A same-order
// margin can be swallowed by that jitter; an order-of-magnitude margin
// (2s vs 100ms) cannot plausibly be, even under heavy concurrent load (see
// that test's own doc comment for how to reproduce and verify this).
type slowFirstQueryStore struct {
	tsdb.Store
	blockFor time.Duration
	slept    int32
}

func (s *slowFirstQueryStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	if atomic.CompareAndSwapInt32(&s.slept, 0, 1) {
		ctx, cancel := context.WithTimeout(context.Background(), s.blockFor)
		defer cancel()
		<-ctx.Done()
	}
	return s.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

// TestOverviewComputeTimeoutFailsAndDoesNotOutliveNegativeTTL is N3: the
// detached compute context is also bounded by computeTimeout, so a hung
// tsdb call cannot leak the computation's goroutine forever - once the
// deadline passes, the next ctx.Err() check downstream
// (clusterTokenRatePoints, called after BuildCluster's per-deployment loop)
// turns it into a real error. That error is a context error
// (context.DeadlineExceeded), so monitor.Cached's own rule for context
// errors applies (see monitor/cache.go): it is never cached at all, not
// even for the short negative TTL - a strictly stronger guarantee than "not
// cached beyond it". The very next call, immediately after and with no
// clock advance at all, must therefore get a fresh attempt rather than
// replay the timeout.
func TestOverviewComputeTimeoutFailsAndDoesNotOutliveNegativeTTL(t *testing.T) {
	original := computeTimeout
	// computeTimeout (2s) and the fake store's blockFor (20s, below) are
	// deliberately an order of magnitude apart - see slowFirstQueryStore's
	// doc comment for why a same-duration pairing was flaky under load.
	// 100ms is still comfortably larger than any real (undelayed)
	// BuildCluster call against this test's tiny fixture ever costs -
	// measured well under 40ms even with -race's per-access
	// instrumentation - so the second call's success is never in doubt
	// regardless of machine load. Verified with
	// `go test -race -count=10 ./internal/server/monitorapi/... -run TestOverviewComputeTimeoutFailsAndDoesNotOutliveNegativeTTL`
	// run concurrently with `go test ./internal/server/app/...` to put the
	// machine under real scheduling load.
	computeTimeout = 2 * time.Second
	t.Cleanup(func() { computeTimeout = original })

	real := newTestStore(t)
	slow := &slowFirstQueryStore{Store: real, blockFor: 20 * time.Second}
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	svc := New(Deps{TSDB: slow, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	_, err := svc.Overview(context.Background(), time.Hour)
	if err == nil {
		t.Fatal("expected an error once the detached compute context's deadline passes")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded (surfaced via a downstream ctx.Err() check)", err)
	}

	// Immediately after, same clock, no advance at all: a context-error
	// result is never cached (monitor/cache.go), so this must be a fresh,
	// successful attempt - the fake store only sleeps on its very first
	// call, already consumed above - not a replay of the timeout.
	if _, err := svc.Overview(context.Background(), time.Hour); err != nil {
		t.Fatalf("Overview immediately after the timeout: %v, want a fresh successful attempt (context errors are never cached)", err)
	}
}
