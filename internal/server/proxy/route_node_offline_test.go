package proxy

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// MODEL_NODE_OFFLINE is the answer only when a running, awake engine is cut
// off by its node's connection and nothing else can take the request: a
// replica still starting beside it does not change that, a sleeping one keeps
// the wake-up answer, and a reachable replica on another node is routed to.
func TestNodeOfflineRefusalOnlyWhenNothingElseAnswers(t *testing.T) {
	type replica struct {
		node  string
		state string
		sleep int
	}
	for _, tc := range []struct {
		name       string
		replicas   []replica
		wantStatus int
		wantSub    string
		wantAsleep bool
		wantNode   string
	}{
		{name: "running on a disconnected node beside a replica still starting",
			replicas:   []replica{{"lost", proto.InstRunning, 0}, {"lost", proto.InstStarting, 0}},
			wantStatus: http.StatusServiceUnavailable, wantSub: "no live agent connection"},
		{name: "asleep on a disconnected node",
			replicas:   []replica{{"lost", proto.InstRunning, 1}},
			wantStatus: http.StatusServiceUnavailable, wantSub: "asleep", wantAsleep: true},
		{name: "running on a disconnected node beside a reachable replica",
			replicas: []replica{{"lost", proto.InstRunning, 0}, {"live", proto.InstRunning, 0}},
			wantNode: "live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:proxy_node_offline_"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := st.Close(); err != nil {
					t.Error(err)
				}
			})
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/offline", Revision: "main", Name: "offline"})
			if err != nil {
				t.Fatal(err)
			}
			nodes := map[string]*store.Node{}
			for _, uuid := range []string{"lost", "live"} {
				node, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid)
				if err != nil {
					t.Fatal(err)
				}
				nodes[uuid] = node
			}
			dep, err := st.CreateDeployment(&store.Deployment{ModelID: model.ID, NodeID: nodes["lost"].ID, ServedName: "offline-model", Engine: "vllm", State: "active", Sleepable: true})
			if err != nil {
				t.Fatal(err)
			}
			for i, r := range tc.replicas {
				if _, err := st.CreateInstance(&store.Instance{
					UUID: r.node + "-" + r.state + "-" + string(rune('a'+i)), DeploymentID: dep.ID, NodeID: nodes[r.node].ID,
					State: r.state, SleepLevel: r.sleep, APIPort: 30001 + i,
				}); err != nil {
					t.Fatal(err)
				}
			}
			p := New(storeplane.New(st), storeNetworks(st), readinessTestHub{online: map[string]bool{"lost": false, "live": true}}, nil, 0)
			t.Cleanup(p.Close)
			_, tgt, status, msg, asleep := p.selectStatus("offline-model")
			if tc.wantNode != "" {
				if status != 0 || tgt.nodeUUID != tc.wantNode {
					t.Fatalf("status=%d message=%q target=%q, want the request routed to %q", status, msg, tgt.nodeUUID, tc.wantNode)
				}
				return
			}
			if status != tc.wantStatus || asleep != tc.wantAsleep || !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("status=%d message=%q asleep=%v, want %d containing %q asleep=%v", status, msg, asleep, tc.wantStatus, tc.wantSub, tc.wantAsleep)
			}
			if !tc.wantAsleep && strings.Contains(msg, "asleep") {
				t.Fatalf("message=%q names sleep for a node that lost its connection", msg)
			}
		})
	}
}
