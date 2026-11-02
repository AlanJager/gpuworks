package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestMonitorPDReadyReplicasMatchRoleSummary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		sleep        int
		prefillSleep int
		follower     string
		distributed  bool
		want         int
	}{
		{name: "awake", want: 2},
		{name: "asleep", sleep: 1, want: 1},
		{name: "all asleep", sleep: 1, prefillSleep: 1, want: 0},
		{name: "missing follower", distributed: true, want: 1},
		{name: "failed follower", distributed: true, follower: proto.InstFailed, want: 1},
		{name: "complete group", distributed: true, follower: proto.InstRunning, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, ts := newMonitorTestStore(t), newMonitorTestTSDB(t)
			node, err := st.RegisterNode("pd-node", store.HashToken("token"), "pd-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.SetNodeState(node.UUID, store.NodeStateOnline); err != nil {
				t.Fatal(err)
			}
			placement := `{"pd":{"mode":"vllm-nixl","roles":{"prefill":{"units":[{"node_uuid":"pd-node"}]},"decode":{"units":[{"node_uuid":"pd-node"}]}}}}`
			followerNodeID := node.ID
			if tc.distributed {
				followerNode, err := st.RegisterNode("pd-follower", store.HashToken("follower-token"), "pd-follower")
				if err != nil {
					t.Fatal(err)
				}
				if err := st.SetNodeState(followerNode.UUID, store.NodeStateOnline); err != nil {
					t.Fatal(err)
				}
				followerNodeID = followerNode.ID
				placement = strings.Replace(placement, `"decode":{"units":[{"node_uuid":"pd-node"}]}`, `"decode":{"units":[{"node_uuid":"pd-node","followers":[{"node_uuid":"pd-follower"}]}]}`, 1)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: "pd", Engine: "vllm", NodeID: node.ID, Replicas: 1, State: store.DeployActive, Placement: placement})
			if err != nil {
				t.Fatal(err)
			}
			ranks := 1
			if tc.distributed {
				ranks = 2
			}
			for _, inst := range []store.Instance{
				{UUID: "prefill", DeploymentID: dep.ID, NodeID: node.ID, Role: "prefill", State: proto.InstRunning, SleepLevel: tc.prefillSleep},
				{UUID: "decode", DeploymentID: dep.ID, NodeID: node.ID, Role: "decode", State: proto.InstRunning, SleepLevel: tc.sleep, NNodes: ranks, DistGroup: "decode-group", DistAttempt: 1},
			} {
				if _, err := st.CreateInstance(&inst); err != nil {
					t.Fatal(err)
				}
			}
			if tc.follower != "" {
				_, err := st.CreateInstance(&store.Instance{UUID: "follower", DeploymentID: dep.ID, NodeID: followerNodeID, Role: "decode", State: tc.follower, NNodes: 2, NodeRank: 1, Headless: true, DistGroup: "decode-group", DistAttempt: 1, DesiredStateJSON: `{"headless":true}`})
				if err != nil {
					t.Fatal(err)
				}
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			pd, err := planning.PDOf(placement)
			if err != nil {
				t.Fatal(err)
			}
			ready := 0
			for _, role := range planning.PDStatusOf(pd, instances).Roles {
				ready += role.Ready
			}
			if ready != tc.want {
				t.Fatalf("PD role summary=%d want %d", ready, tc.want)
			}
			svc := newMonitorAPIService(st, ts, nil, nil, nil, nil)
			overview, err := svc.Overview(context.Background(), time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if len(overview.Services) != 1 {
				t.Fatalf("services=%+v", overview.Services)
			}
			detail, err := svc.Deployment(context.Background(), dep.ID, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range map[string]any{"overview": overview.Services[0].Replicas, "detail": detail.Deployment.Replicas} {
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				var wire map[string]int
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				if len(wire) != 2 || wire["ready"] != tc.want || wire["desired"] != 2 {
					t.Errorf("%s replicas=%s; PD list role summary=%d/2", name, raw, ready)
				}
			}
			signal := overview.Signals[0]
			if signal.Key != "ready_replicas" || signal.Value == nil || *signal.Value != float64(tc.want) || signal.Denominator == nil || *signal.Denominator != 2 {
				t.Errorf("overview signal=%+v", signal)
			}
		})
	}
}
