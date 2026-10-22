package monitorapi

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/monitor"
)

// This file covers dto.MonitorAlignment's in_flight/discrepancy_persistent
// fields and the switch from RequestsPerSec/ErrRate*-based approximation to
// exact cumulative-counter-delta accounting for proxy_requests/
// client_errors/server_errors/engine_completed (see buildAlignment's doc
// comment in deployment.go and monitor.Window.ProxyRequestsDelta's).
// Split out from deployment_test.go rather than appended there, which is
// already at CLAUDE.md's 600-line split threshold.
//
// InFlight is never subtracted from Lost: proxy/accounting.go only
// increments gpuworks_proxy_requests_total at Finish/RecordImmediate, so a
// request still in flight was never counted by ProxyRequests (or
// EngineCompleted) in the first place - there is nothing to net out of an
// equation it was never part of. DiscrepancyPersistent instead treats
// InflightMax as an upper bound on how much of Lost a "still streaming"
// explanation could possibly cover.

// TestBuildAlignmentLostIsNotReducedByInFlight is F3's exact case: Lost is
// proxy_requests - engine_completed - client_errors - server_errors, full
// stop - in_flight plays no part in the arithmetic even when it is large
// enough that subtracting it would zero Lost out.
func TestBuildAlignmentLostIsNotReducedByInFlight(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 10, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 10}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	proxyLabels := map[string]string{"deployment": "10", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 100, baseTime)

	successLabels := map[string]string{"deployment": "10", "instance": "i1"}
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 70, baseTime)

	inflightLabels := map[string]string{"deployment": "10"}
	writeSample(t, store, "gpuworks_proxy_requests_inflight", inflightLabels, 30, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_inflight", inflightLabels, 30, baseTime)

	out, err := svc.Deployment(context.Background(), 10, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated")
	}
	a := out.Alignment
	if a.ProxyRequests != 100 {
		t.Errorf("ProxyRequests = %d, want 100", a.ProxyRequests)
	}
	if a.EngineCompleted != 70 {
		t.Errorf("EngineCompleted = %d, want 70", a.EngineCompleted)
	}
	if a.InFlight != 30 {
		t.Errorf("InFlight = %d, want 30", a.InFlight)
	}
	if a.Lost != 30 {
		t.Errorf("Lost = %d, want 30 (100-70-0-0, in_flight not subtracted)", a.Lost)
	}
	// Lost (30) is not greater than InflightMax (30): the gap could, at
	// most, be fully explained by requests still in flight, so this is not
	// (yet) a persistent discrepancy.
	if a.DiscrepancyPersistent {
		t.Error("DiscrepancyPersistent = true, want false (Lost == InflightMax, not >)")
	}
}

// TestBuildAlignmentDiscrepancyPersistentWhenLostExceedsInFlight is F5's
// "true" branch: Lost strictly exceeds the largest in-flight count observed
// anywhere in the window, so even crediting every concurrently open request
// at its peak, the gap does not close.
func TestBuildAlignmentDiscrepancyPersistentWhenLostExceedsInFlight(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 11, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 11}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	proxyLabels := map[string]string{"deployment": "11", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 50, baseTime)

	successLabels := map[string]string{"deployment": "11", "instance": "i1"}
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 20, baseTime)

	// In-flight peaked at 10 mid-window, well under the 30-request gap.
	inflightLabels := map[string]string{"deployment": "11"}
	writeSample(t, store, "gpuworks_proxy_requests_inflight", inflightLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_inflight", inflightLabels, 10, baseTime)

	out, err := svc.Deployment(context.Background(), 11, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated")
	}
	a := out.Alignment
	if a.Lost != 30 {
		t.Errorf("Lost = %d, want 30 (50-20-0-0)", a.Lost)
	}
	if a.InFlight != 10 {
		t.Errorf("InFlight = %d, want 10 (the last sample)", a.InFlight)
	}
	if !a.DiscrepancyPersistent {
		t.Error("DiscrepancyPersistent = false, want true (Lost 30 > InflightMax 10)")
	}
}

// TestBuildAlignmentNotDiscrepancyPersistentWithoutInflightData is F5's
// "no inflight series" branch: even with Lost > 0, DiscrepancyPersistent
// must be false when the in-flight gauge was never collected at all - there
// is nothing to compare Lost against.
func TestBuildAlignmentNotDiscrepancyPersistentWithoutInflightData(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 12, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 12}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	proxyLabels := map[string]string{"deployment": "12", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 50, baseTime)

	successLabels := map[string]string{"deployment": "12", "instance": "i1"}
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 20, baseTime)
	// Deliberately no gpuworks_proxy_requests_inflight sample at all.

	out, err := svc.Deployment(context.Background(), 12, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated")
	}
	a := out.Alignment
	if a.Lost != 30 {
		t.Errorf("Lost = %d, want 30 (50-20-0-0)", a.Lost)
	}
	if a.InFlight != 0 {
		t.Errorf("InFlight = %d, want 0 (no data collected)", a.InFlight)
	}
	if a.DiscrepancyPersistent {
		t.Error("DiscrepancyPersistent = true, want false (no in-flight series to compare Lost against)")
	}
}

// TestBuildAlignmentCounterResetMidWindow proves EngineCompleted (queried
// directly by buildAlignment, not carried on Window) uses the same
// reset-aware delta as ProxyRequests: a mid-window engine restart must not
// silently under-count, or worse, contribute a negative delta.
func TestBuildAlignmentCounterResetMidWindow(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 13, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 13}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime.Add(30 * time.Second) }})

	proxyLabels := map[string]string{"deployment": "13", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 0, baseTime)
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 200, baseTime.Add(30*time.Second))

	// Engine restarts mid-window: counter climbs to 100, resets to 10 (the
	// new process's counter starting over), then climbs to 50. Naive
	// first-vs-last math would read 50-0=50; walking every step with reset
	// handling gives 100 (0->100) + 10 (reset) + 40 (10->50) = 150.
	successLabels := map[string]string{"deployment": "13", "instance": "i1"}
	successValues := []float64{0, 100, 10, 50}
	for i, v := range successValues {
		writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, v, baseTime.Add(time.Duration(i)*10*time.Second))
	}

	out, err := svc.Deployment(context.Background(), 13, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated")
	}
	if got, want := out.Alignment.EngineCompleted, int64(150); got != want {
		t.Errorf("EngineCompleted = %d, want %d (reset-aware, not naive first-vs-last 50)", got, want)
	}
}

// An engine that reports no completion counter at all (SGLang without
// --enable-metrics) leaves the proxy nothing to reconcile against: the
// alignment row is absent rather than a "lost" count equal to every request.
func TestBuildAlignmentIsAbsentWithoutAnEngineCounter(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 12, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 12}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})
	proxyLabels := map[string]string{"deployment": "12", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", proxyLabels, 566, baseTime)

	out, err := svc.Deployment(context.Background(), 12, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment != nil {
		t.Fatalf("Alignment = %+v, want nil without an engine counter", out.Alignment)
	}
}
