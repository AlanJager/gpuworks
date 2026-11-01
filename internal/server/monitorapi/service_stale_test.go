package monitorapi

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
)

// A cached overview served after its TTL, while the rebuild runs, must not
// pass for a fresh one: it keeps the as_of it was computed for, and stale is
// judged again at the time it is served.
func TestOverviewServedFromBeforeTheRefreshKeepsItsAsOfAndGoesStale(t *testing.T) {
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc", Engine: "vllm", ReplicasDesired: 1}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 1, State: proto.InstRunning}},
	}
	real := newTestStore(t)
	labels := map[string]string{"instance": "i1", "deployment": "1"}
	writeSample(t, real, "gpuworks_infer_requests_running", labels, 4, baseTime)

	var block atomic.Bool
	release := make(chan struct{})
	blocked := make(chan struct{}, 64)
	store := &blockingStore{Store: real, onFirstQuery: func() {
		if block.Load() {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-release
		}
	}}
	var nowNS atomic.Int64
	nowNS.Store(baseTime.UnixNano())
	clock := func() time.Time { return time.Unix(0, nowNS.Load()) }
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: clock})

	first, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if first.Stale || first.AsOf != baseTime.UnixMilli() {
		t.Fatalf("first overview: stale=%v as_of=%d, want fresh as of %d", first.Stale, first.AsOf, baseTime.UnixMilli())
	}

	// 40 s later the cache is past its TTL (so a rebuild starts, which blocks)
	// and the newest sample is more than 30 s old.
	block.Store(true)
	nowNS.Store(baseTime.Add(40 * time.Second).UnixNano())
	type result struct {
		out dto.MonitorOverview
		err error
	}
	served := make(chan result, 1)
	go func() {
		out, err := svc.Overview(context.Background(), time.Hour)
		served <- result{out, err}
	}()
	var second result
	select {
	case second = <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the overview waited for the rebuild instead of serving the cached one")
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	// The wire says what the old snapshot was built from, and that it is old.
	if second.out.AsOf != first.AsOf || !second.out.Stale {
		t.Fatalf("overview served during the rebuild: as_of=%d stale=%v, want as_of %d and stale", second.out.AsOf, second.out.Stale, first.AsOf)
	}

	// The rebuild, once it can finish, replaces it with one built from newer data.
	writeSample(t, real, "gpuworks_infer_requests_running", labels, 5, baseTime.Add(40*time.Second))
	block.Store(false)
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := svc.Overview(context.Background(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if out.AsOf == baseTime.Add(40*time.Second).UnixMilli() {
			if out.Stale {
				t.Fatalf("refreshed overview is stale: %+v", out)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refresh never replaced the overview (as_of=%d)", out.AsOf)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
