package proxy

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRouteTableEventRebuildBypassesRequestThrottle(t *testing.T) {
	r := &routeTable{dirty: make(chan struct{}, 1)}
	r.lastRebuild.Store(time.Now().UnixNano())
	r.requestRebuild()
	select {
	case <-r.dirty:
		t.Fatal("request-path rebuild ignored the throttle window")
	default:
	}
	r.requestEventRebuild()
	select {
	case <-r.dirty:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("bus event was lost during the throttle window")
	}
}

func TestRouteTableCloseAndRestartStopsRefreshLoop(t *testing.T) {
	st, err := store.Open("file:route_table_lifecycle_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := newRouteTable(st, storeNetworks(st), readinessTestHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.close()
	r.mu.Lock()
	runningAfterClose := r.running
	r.mu.Unlock()
	if runningAfterClose {
		t.Fatal("route table remains marked running after close")
	}
	r.start()
	r.close()
}

func TestRouteTableBuildKeepsOnlyReadyServingCandidates(t *testing.T) {
	st, err := store.Open("file:route_table_candidates_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("route-node", store.HashToken("route-node"), "route-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "model", Engine: "vllm", State: store.DeployActive, RoutingPolicy: "least_request"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "ready", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, APIPort: 30001, Ports: "[30001]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "starting", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstStarting, APIPort: 30002, Ports: "[30002]"}); err != nil {
		t.Fatal(err)
	}
	r := newRouteTable(st, storeNetworks(st), readinessTestHub{online: map[string]bool{node.UUID: true}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.close()
	rows, ok := r.snapshot("model")
	if !ok || len(rows) != 1 || len(rows[0].candidates) != 1 || rows[0].candidates[0].instanceUUID != "ready" {
		t.Fatalf("route snapshot=%+v, want one ready candidate", rows)
	}
}

// A routine node status report waits for the periodic refresh; every other
// non-progress event rebuilds at once.
func TestRouteTableRebuildsOnEventsThatChangeRouting(t *testing.T) {
	cases := []struct {
		e    bus.Event
		want bool
	}{
		{bus.Event{Topic: "node", Reason: store.NodeChangeStatus}, false},
		{bus.Event{Topic: "node", Reason: "state:offline"}, true},
		{bus.Event{Topic: "instance", Reason: "status"}, true},
		{bus.Event{Topic: "instance", Reason: bus.ReasonProgress}, false},
		{bus.Event{Topic: "deployment", Reason: "updated"}, true},
	}
	for _, c := range cases {
		if got := rebuildsOn(c.e); got != c.want {
			t.Errorf("rebuildsOn(%s/%s) = %v, want %v", c.e.Topic, c.e.Reason, got, c.want)
		}
	}
}

// An event inside routeEventRebuildGap of the last rebuild is not lost: it
// rebuilds once the gap has passed.
func TestRouteTableDefersEventRebuildsWithinTheGap(t *testing.T) {
	st, err := store.Open("file:route_table_gap_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := newRouteTable(st, storeNetworks(st), readinessTestHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.close()
	r.rebuild()
	before := r.lastRebuild.Load()
	r.requestEventRebuild()
	time.Sleep(routeEventRebuildGap / 5)
	if r.lastRebuild.Load() != before {
		t.Fatal("rebuilt inside the gap")
	}
	deadline := time.Now().Add(routeEventRebuildGap + time.Second)
	for r.lastRebuild.Load() == before {
		if time.Now().After(deadline) {
			t.Fatal("the deferred rebuild never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A change that may alter which instances route rebuilds at once; an
// instance no request can reach yet waits out the gap.
func TestRouteTableUrgentEvents(t *testing.T) {
	r := &routeTable{}
	r.current.Store(&routeSnapshot{running: map[string]bool{"live": true}})
	cases := []struct {
		e    bus.Event
		want bool
	}{
		{bus.Event{Topic: "instance", Key: "live", Reason: proto.InstDraining}, true},
		{bus.Event{Topic: "instance", Key: "live", Reason: "deleted"}, true},
		{bus.Event{Topic: "instance", Key: "new", Reason: proto.InstRunning}, true},
		{bus.Event{Topic: "instance", Key: "new", Reason: "sleep"}, true},
		{bus.Event{Topic: "instance", Key: "new", Reason: "created"}, false},
		{bus.Event{Topic: "instance", Key: "new", Reason: proto.InstStarting}, false},
		{bus.Event{Topic: "deployment", Key: "1", Reason: "updated"}, true},
		{bus.Event{Topic: "node", Key: "n", Reason: "state:offline"}, true},
	}
	for _, c := range cases {
		if got := r.urgentOn(c.e); got != c.want {
			t.Errorf("urgentOn(%s %s %s) = %v, want %v", c.e.Topic, c.e.Key, c.e.Reason, got, c.want)
		}
	}
}

// The snapshot remembers which instances it saw running.
func TestRouteTableSnapshotRecordsRunningInstances(t *testing.T) {
	st, err := store.Open("file:route_table_running_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/m", Revision: "main", Name: "m"})
	node, _ := st.RegisterNode("route-running", store.HashToken("route-running"), "route-running")
	dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: node.ID, ServedName: "m", Engine: "vllm", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	for uuid, state := range map[string]string{"up": proto.InstRunning, "booting": proto.InstStarting} {
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: state, APIPort: 30001}); err != nil {
			t.Fatal(err)
		}
	}
	r := newRouteTable(st, storeNetworks(st), readinessTestHub{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer r.close()
	snap := r.current.Load()
	if !snap.running["up"] || snap.running["booting"] {
		t.Fatalf("running = %v", snap.running)
	}
}
