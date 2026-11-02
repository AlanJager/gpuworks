package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

type blockingHygonCUProber struct {
	*hygonTestProber
	inventories chan struct{}
	entered     chan struct{}
	release     <-chan struct{}
	queries     atomic.Int32
	active      atomic.Int32
	peak        atomic.Int32
}

func (p *blockingHygonCUProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	p.inventories <- struct{}{}
	return p.hygonTestProber.Probe(ctx)
}

func (p *blockingHygonCUProber) ComputeUnits(ctx context.Context, _ int) (int, error) {
	p.queries.Add(1)
	active := p.active.Add(1)
	defer p.active.Add(-1)
	for peak := p.peak.Load(); active > peak; peak = p.peak.Load() {
		if p.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	p.entered <- struct{}{}
	select {
	case <-p.release:
		return 0, errors.New("stop before native creation")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func TestHygonPhysicalQueriesAreSerializedWithAllocation(t *testing.T) {
	e, h, helper := hygonTestEngine(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	p := &blockingHygonCUProber{hygonTestProber: e.prober.(*hygonTestProber), inventories: make(chan struct{}, 8), entered: make(chan struct{}, 8), release: release}
	e.prober = p
	var workers sync.WaitGroup
	for i := range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			h.apply(e, t.Context(), &agentruntime.WorkloadSpec{}, proto.InstanceDesiredState{
				UUID: fmt.Sprintf("parallel-%d", i), GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20,
			})
		}()
	}
	for range 8 {
		select {
		case <-p.inventories:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel inventory calls did not start")
		}
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first physical query did not start")
	}
	// Other requests have completed inventory and can reach the query while
	// the first is blocked. They must wait without starting another vendor CLI.
	timer := time.NewTimer(100 * time.Millisecond)
	select {
	case <-p.entered:
	case <-timer.C:
	}
	timer.Stop()
	peak := p.peak.Load()
	unblock()
	workers.Wait()
	if peak != 1 || p.queries.Load() != 8 || len(helper.createdCUs) != 0 {
		t.Fatalf("parallel queries=%d total=%d native creates=%v", peak, p.queries.Load(), helper.createdCUs)
	}
}

func TestHygonCanceledQueuedAllocationDoesNotQueryOrCreate(t *testing.T) {
	e, h, helper := hygonTestEngine(t)
	release := make(chan struct{})
	p := &blockingHygonCUProber{hygonTestProber: e.prober.(*hygonTestProber), inventories: make(chan struct{}, 1), entered: make(chan struct{}, 1), release: release}
	e.prober = p
	e.partitionMu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(e.partitionMu.Unlock) }
	t.Cleanup(unlock)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		ok, _ := h.apply(e, ctx, &agentruntime.WorkloadSpec{}, proto.InstanceDesiredState{
			UUID: "canceled-queued", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20,
		})
		done <- ok
	}()
	select {
	case <-p.inventories:
	case <-time.After(5 * time.Second):
		t.Fatal("queued allocation did not start inventory")
	}
	cancel()
	unlock()
	select {
	case ok := <-done:
		if ok || p.queries.Load() != 0 || len(helper.createdCUs) != 0 {
			t.Fatalf("canceled request ok=%v queries=%d native creates=%v", ok, p.queries.Load(), helper.createdCUs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled queued allocation did not return")
	}
}
