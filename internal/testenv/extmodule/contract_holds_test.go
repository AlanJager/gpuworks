package extmodule

import (
	"context"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// reportCards writes one status report of a node with n cards, the way its
// agent would. The nodes below have no agent, so this is the only report
// they ever get: what changes them afterwards is not a status report.
func reportCards(t *testing.T, e *testenv.Env, uuid string, n int) {
	t.Helper()
	ok := true
	cards := make([]proto.GPUInfo, n)
	for i := range cards {
		cards[i] = proto.GPUInfo{Index: i, UUID: "GPU-" + uuid + "-" + string(rune('a'+i)), Vendor: "nvidia",
			PCIeBDF: "0000:" + string(rune('3'+i)) + "f:00.0", MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	if err := e.Store().UpdateNodeStatus(uuid, proto.NodeStatus{GPUProbeOK: &ok, GPUs: cards, Sys: proto.SysInfo{RuntimeDriver: "docker"}}); err != nil {
		t.Fatal(err)
	}
}

func schedulingOf(t *testing.T, e *testenv.Env, uuid string) (store.NodeScheduling, store.SchedulingSource) {
	t.Helper()
	n, err := e.Store().NodeByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	return n.Scheduling, n.SchedulingBy
}

// A holder whose answer changes asks for a pass: the node is cordoned, and
// released, within moments rather than at its next status report (a minute
// away; this node has no agent and never reports again).
func TestAHolderReappliesItsHoldsOnDemand(t *testing.T) {
	holder := &fakeHolder{}
	mod := &fakeModule{name: "fake", holder: holder}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	n, _, err := e.Store().CreateNodePending("held")
	if err != nil {
		t.Fatal(err)
	}
	reportCards(t, e, n.UUID, 2)
	if s, by := schedulingOf(t, e, n.UUID); s != store.NodeSchedulable {
		t.Fatalf("node before the hold = %s by %q", s, by)
	}

	holder.on.Store(true)
	asked := time.Now()
	mod.host.ReapplySchedulingHolds()
	e.Eventually(t, 3*time.Second, func() bool {
		s, by := schedulingOf(t, e, n.UUID)
		return s == store.NodeCordoned && by == "fakehold"
	})
	if took := time.Since(asked); took > 3*time.Second {
		t.Fatalf("the hold took %s", took)
	}
	holder.on.Store(false)
	mod.host.ReapplySchedulingHolds()
	e.Eventually(t, 5*time.Second, func() bool {
		s, by := schedulingOf(t, e, n.UUID)
		return s == store.NodeSchedulable && by == store.SchedulingBySelf
	})
}

// A burst of requests makes a bounded number of passes, and the last
// answer wins: the requests made after the holder flipped back are served
// by a pass that follows the burst.
func TestABurstOfReapplyRequestsMakesBoundedPasses(t *testing.T) {
	holder := &fakeHolder{}
	mod := &fakeModule{name: "fake", holder: holder}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	n, _, err := e.Store().CreateNodePending("flapping")
	if err != nil {
		t.Fatal(err)
	}
	reportCards(t, e, n.UUID, 1)
	before := holder.asked.Load() // one node: each pass asks the holder once

	const calls = 500
	holder.on.Store(true)
	for range calls {
		mod.host.ReapplySchedulingHolds()
	}
	holder.on.Store(false)
	for range calls {
		mod.host.ReapplySchedulingHolds()
	}
	// The burst took far less than the two-second floor between passes:
	// at most a pass at once and one after the floor.
	time.Sleep(5 * time.Second)
	passes := holder.asked.Load() - before
	if passes < 1 || passes > 2 {
		t.Fatalf("%d requests made %d passes, want 1 or 2", 2*calls, passes)
	}
	if s, by := schedulingOf(t, e, n.UUID); s != store.NodeSchedulable || by != store.SchedulingBySelf {
		t.Fatalf("after the burst the node is %s by %q; the last answer (not held) must win", s, by)
	}
}

// A module learns the fleet from the Host: NodeFleet reads every node's
// hold facts, and store.NodeFleetTopic announces each insert, card-count
// change and delete by node id, read with store.NodeFleetChangeOf.
func TestAModuleLearnsTheFleetFromTheHost(t *testing.T) {
	mod := &fakeModule{name: "fake", holder: &fakeHolder{}}
	e := testenv.NewWithExtensions(t, app.Extensions{Modules: []app.Module{mod}}, nil)
	sub := mod.host.Bus().Subscribe(64, store.NodeFleetTopic)
	defer sub.Close()
	next := func() store.NodeFleetChange {
		t.Helper()
		for {
			select {
			case ev := <-sub.C():
				if c, ok := store.NodeFleetChangeOf(ev); ok {
					return c
				}
				t.Fatalf("unreadable fleet event %+v", ev)
			case <-time.After(5 * time.Second):
				t.Fatal("no fleet event")
			}
		}
	}
	fleet := func() []store.NodeHoldFacts {
		t.Helper()
		facts, err := mod.host.NodeFleet(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return facts
	}
	if got := fleet(); len(got) != 0 {
		t.Fatalf("fleet before any node = %+v", got)
	}

	api := e.API(t)
	if status, body := send(t, api, http.MethodPost, "/api/v1/nodes", `{"name":"fleet","server_url":"http://127.0.0.1:1"}`); status/100 != 2 {
		t.Fatalf("add node = %d %s", status, body)
	}
	inserted := next()
	got := fleet()
	if inserted.Kind != store.NodeInserted || len(got) != 1 || got[0].ID != inserted.NodeID || got[0].OwnedCards != 0 {
		t.Fatalf("insert: event %+v, fleet %+v", inserted, got)
	}
	uuid := got[0].UUID

	reportCards(t, e, uuid, 2)
	if c := next(); c != (store.NodeFleetChange{NodeID: inserted.NodeID, Kind: store.NodeFactsChanged}) {
		t.Fatalf("card count change: event %+v", c)
	}
	if got := fleet(); len(got) != 1 || got[0].OwnedCards != 2 || got[0].GPUVendor != "nvidia" {
		t.Fatalf("fleet after the report = %+v", got)
	}
	reportCards(t, e, uuid, 2) // the same facts: no event
	select {
	case ev := <-sub.C():
		t.Fatalf("a report that changed no fact was announced: %+v", ev)
	case <-time.After(200 * time.Millisecond):
	}

	if status, body := send(t, api, http.MethodDelete, "/api/v1/nodes/"+uuid+"?force=true", ""); status/100 != 2 {
		t.Fatalf("delete node = %d %s", status, body)
	}
	if c := next(); c != (store.NodeFleetChange{NodeID: inserted.NodeID, Kind: store.NodeDeleted}) {
		t.Fatalf("delete: event %+v", c)
	}
	if got := fleet(); len(got) != 0 {
		t.Fatalf("fleet after the delete = %+v", got)
	}
	if _, ok := store.NodeFleetChangeOf(bus.Event{Topic: "node", Key: uuid, Reason: "deleted:1"}); ok {
		t.Fatal("a node topic event read as a fleet change")
	}
}
