package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A node page's stats count only the deployment's instances on that node;
// the fleet's count all of them. The shared deployment runs one replica on
// node A (awake for the first hour, then asleep) and one on node B (from +1h,
// awake for an hour, then asleep), which yield one and two times and are
// woken zero and one times: each node's drawer shows its own replica's
// awake and present time and its own yields, and the fleet's table the sum.
func TestNodeTimelineCountsOnlyThatNodesReplicas(t *testing.T) {
	r := newTimelineRig(t)
	nodeB, err := r.st.RegisterNode("tl-node-b", store.HashToken("token-b"), "tl-node-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.DB().Create(&store.GPU{NodeID: nodeB.ID, Ident: "b0", Index: 0, Name: "RTX 8000", MemoryTotal: 48 << 30}).Error; err != nil {
		t.Fatal(err)
	}
	if err := r.st.DB().Create(&store.Instance{UUID: "inst-shared-b", DeploymentID: r.shared.ID, NodeID: nodeB.ID, State: proto.InstRunning, GPUIndexes: "[0]", CreatedAt: r.t0.Add(time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	sleepAt := func(uuid string, at time.Duration) {
		r.event(uuid, r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", at, `{"state":"running","phase":"sleep"}`)
		r.event(uuid, r.shared.ID, "sleep", "INSTANCE_SLEEP_RESULT", at, `{"result":"applied","level":1}`)
		r.event(uuid, r.shared.ID, "state", "INSTANCE_STATUS_UPDATED", at, `{"state":"running","phase":""}`)
	}
	r.event("inst-shared", r.shared.ID, "state", "INSTANCE_CREATED", 0, `{"state":"running"}`)
	sleepAt("inst-shared", time.Hour)
	r.event("inst-shared-b", r.shared.ID, "state", "INSTANCE_CREATED", time.Hour, `{"state":"running"}`)
	sleepAt("inst-shared-b", 2*time.Hour)
	yield := func(uuid string, at time.Duration) {
		r.event("", r.shared.ID, store.EventKindAutomation, "AUTO_SLEEP", at, fmt.Sprintf(`{"trigger":"preempt","reason":"turn","for_deployment":%d,"level":1,"instances":[%q]}`, r.plain.ID, uuid))
	}
	yield("inst-shared", time.Hour)
	yield("inst-shared-b", 90*time.Minute)
	yield("inst-shared-b", 2*time.Hour)
	r.event("", r.shared.ID, store.EventKindAutomation, "AUTO_WAKE", 100*time.Minute, `{"trigger":"request","instances":["inst-shared-b"]}`)

	type figures struct {
		scope                    string
		awake, present           int64
		yielded, wakes, switches int
	}
	read := func(query string) figures {
		t.Helper()
		status, body := r.get(query)
		var got dto.SchedulingTimeline
		if status != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil {
			t.Fatalf("%s = %d %s", query, status, body)
		}
		for _, d := range got.Deployments {
			if d.DeploymentID == r.shared.ID {
				return figures{got.Scope, d.AwakeSeconds, d.PresentSeconds, d.Yielded, d.Wakes, d.Switches}
			}
		}
		t.Fatalf("%s lists no row for the shared deployment: %s", query, body)
		return figures{}
	}
	now := time.Now().UTC()
	sinceT0 := func(d time.Duration) int64 { return int64(now.Sub(r.t0.Add(d)).Seconds()) }
	hour := int64(time.Hour / time.Second)

	a := read("?node=" + r.node.UUID + "&window=6h")
	b := read("?node=" + nodeB.UUID + "&window=6h")
	fleet := read("?sharing=1&window=6h")
	both := read("?node=" + r.node.UUID + "&node=" + nodeB.UUID + "&window=6h")

	if a.scope != dto.TimelineScopeNode || b.scope != dto.TimelineScopeNode || both.scope != dto.TimelineScopeNode || fleet.scope != dto.TimelineScopeFleet {
		t.Fatalf("scopes: a %q, b %q, both %q, fleet %q", a.scope, b.scope, both.scope, fleet.scope)
	}
	// Node A: its replica, awake for the first hour, present since t0, one yield.
	if a.awake != hour || abs64(a.present-sinceT0(0)) > 2 || a.yielded != 1 || a.wakes != 0 || a.switches != 1 {
		t.Errorf("node A = %+v, want awake %d, present ~%d, 1 yield, no wake", a, hour, sinceT0(0))
	}
	// Node B: its replica, awake for an hour, present since +1h, two yields
	// and the wake.
	if b.awake != hour || abs64(b.present-sinceT0(time.Hour)) > 2 || b.yielded != 2 || b.wakes != 1 || b.switches != 3 {
		t.Errorf("node B = %+v, want awake %d, present ~%d, 2 yields, 1 wake", b, hour, sinceT0(time.Hour))
	}
	// The fleet: the sum of both replicas' yields and wakes, awake for the two
	// disjoint hours, present since its first replica.
	if fleet.awake != 2*hour || abs64(fleet.present-sinceT0(0)) > 2 || fleet.yielded != 3 || fleet.wakes != 1 || fleet.switches != 4 {
		t.Errorf("fleet = %+v, want awake %d, present ~%d, 3 yields, 1 wake", fleet, 2*hour, sinceT0(0))
	}
	if fleet.yielded != a.yielded+b.yielded || fleet.wakes != a.wakes+b.wakes || fleet.awake != a.awake+b.awake {
		t.Errorf("fleet %+v is not the sum of node A %+v and node B %+v", fleet, a, b)
	}
	// A page naming both nodes counts each event once: the fleet's counts
	// here, every replica being on one of them.
	if both.yielded != 3 || both.wakes != 1 || both.awake != 2*hour {
		t.Errorf("both nodes = %+v, want the fleet's counts %+v", both, fleet)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
