package engine

import (
	"context"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The server re-resolves a running instance's observation when its template
// changes and sends only that. The agent swaps it on the instance it holds and
// does nothing else: no inspect, no re-probe, no state report - re-adopting
// the container could report it starting or reclaim it for a change that
// touches nothing it does. An update for an instance it does not hold must not
// start one.
func TestUpdateObservationsSwapsTheObservationOfAHeldInstanceOnly(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("observed", healthyEngine(t))
	spec.MetricPath, spec.MetricPrefix = "/metrics", "gpuworks_callable"
	serve(t, e, log, spec)
	waitIdle(t, e, spec.UUID)

	name := containerName(spec.UUID)
	driver.mu.Lock()
	runs, inspects := driver.runs, driver.inspects[name]
	driver.mu.Unlock()
	reports := len(log.snapshot())

	observation := &proto.InstanceObservation{
		MetricPath:  "/stats",
		MetricRules: []proto.MetricRule{{Source: "tensorfold:requests_running", Series: "gpuworks_infer_requests_running"}},
	}
	e.UpdateObservations([]proto.ObservationUpdate{
		{UUID: spec.UUID, Observation: observation, MetricPath: "/stats", MetricPrefix: "gpuworks_callable",
			MetricMap: map[string]string{"tensorfold:requests_running": "gpuworks_infer_requests_running"}},
		{UUID: "never-held", Observation: observation},
	})

	held := e.MetricSpecs()
	if len(held) != 1 || held[0].UUID != spec.UUID {
		t.Fatalf("metric specs = %+v; an update for an unknown instance must not add one", held)
	}
	got := held[0]
	if !reflect.DeepEqual(got.Observation, observation) || got.MetricPath != "/stats" ||
		got.MetricMap["tensorfold:requests_running"] != "gpuworks_infer_requests_running" {
		t.Fatalf("observation not applied: %+v", got)
	}
	// Everything else is the spec the instance was started with.
	restored := got
	restored.Observation, restored.MetricPath, restored.MetricPrefix, restored.MetricMap = nil, spec.MetricPath, spec.MetricPrefix, nil
	if !reflect.DeepEqual(restored, spec) {
		t.Fatalf("launch fields changed:\n got %+v\nwant %+v", restored, spec)
	}

	// Give a wrongly started ensure the time to show itself.
	time.Sleep(100 * time.Millisecond)
	driver.mu.Lock()
	runsAfter, inspectsAfter := driver.runs, driver.inspects[name]
	_, unknownStarted := driver.containers[containerName("never-held")]
	driver.mu.Unlock()
	if runsAfter != runs || inspectsAfter != inspects || unknownStarted {
		t.Fatalf("runtime touched: runs %d->%d, inspects %d->%d, unknown started %v", runs, runsAfter, inspects, inspectsAfter, unknownStarted)
	}
	if after := log.snapshot(); len(after) != reports {
		t.Fatalf("state reports after an observation update: %+v", after[reports:])
	}

	// An empty update is a no-op.
	e.UpdateObservations(nil)
	if again := e.MetricSpecs(); !reflect.DeepEqual(again[0], got) {
		t.Fatal("an empty update changed the instance")
	}
}

// A state.sync after an in-place update carries the observation in the stored
// desired state, so the agent ends up with the same observation either way.
func TestStateSyncCarriesTheObservation(t *testing.T) {
	driver := newLivenessDriver()
	e, log := newLivenessEngine(t, driver)
	spec := runningSpec("synced", healthyEngine(t))
	spec.Observation = &proto.InstanceObservation{MetricRules: []proto.MetricRule{{Source: "a", Series: "gpuworks_infer_requests_running"}}}
	e.SetDesiredFull(context.Background(), []proto.InstanceDesiredState{spec})
	log.waitFor(t, proto.InstRunning, 1)
	held := e.MetricSpecs()
	if len(held) != 1 || !reflect.DeepEqual(held[0].Observation, spec.Observation) {
		t.Fatalf("metric specs = %+v", held)
	}
}
