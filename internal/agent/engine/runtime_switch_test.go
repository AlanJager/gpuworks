package engine

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"log/slog"
	"testing"
)

// Idle is what lets the agent switch runtimes on its own: a switch
// recreates every container, so it must hold only when nothing would be
// recreated — and an agent the control plane has not yet told what it
// runs does not know that it runs nothing.
func TestIdleOnlyWhenNothingRunsStartsOrAwaitsCleanup(t *testing.T) {
	e := newHermeticEngine(&headlessTestDriver{name: "udocker"}, nil, slog.New(slog.DiscardHandler), t.TempDir())
	set := func(mutate func()) {
		e.mu.Lock()
		defer e.mu.Unlock()
		mutate()
	}
	if e.Idle() {
		t.Fatal("idle before the desired state is known")
	}
	set(func() { e.desiredKnown = true })
	if !e.Idle() {
		t.Fatal("not idle with nothing desired")
	}
	for _, state := range []string{"running", "sleeping"} {
		set(func() { e.desired = map[string]proto.InstanceDesiredState{"a": {UUID: "a", DesiredState: state}} })
		if e.Idle() {
			t.Fatalf("idle with a %s instance", state)
		}
	}
	set(func() { e.desired = map[string]proto.InstanceDesiredState{"a": {UUID: "a", DesiredState: "stopped"}} })
	if !e.Idle() {
		t.Fatal("a stopped instance whose cleanup is done kept the node busy")
	}
	set(func() { e.inFlight["a"] = true })
	if e.Idle() {
		t.Fatal("idle while an instance is starting or being torn down")
	}
	set(func() {
		delete(e.inFlight, "a")
		e.cleanupRequired["a"] = proto.InstanceDesiredState{UUID: "a"}
	})
	if e.Idle() {
		t.Fatal("idle with a cleanup pending")
	}
}
