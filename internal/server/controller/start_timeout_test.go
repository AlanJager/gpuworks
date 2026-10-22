package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An instance's health budget is decided once, when its generation is
// written: the deployment's own value, else the cluster default as it reads
// then, with the harness override ahead of both. The agent only ever sees the
// number in the desired state, so that is what each case asserts.
func TestInstanceStartTimeoutAtScheduling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		policy     int // cluster default to store; 0 keeps the shipped one
		deployment int
		harness    int
		want       int
	}{
		{name: "shipped default", want: 1800},
		{name: "cluster default changed", policy: 2400, want: 2400},
		{name: "deployment states its own", policy: 2400, deployment: 3600, want: 3600},
		{name: "harness override wins", policy: 2400, deployment: 3600, harness: 5, want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open("file:controller_start_timeout_"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if tc.policy > 0 {
				policy := st.DeploymentPolicy()
				policy.StartTimeoutSec = tc.policy
				raw, _ := json.Marshal(policy)
				if err := st.SetSetting(store.SettingDeploymentPolicy, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			node, err := st.RegisterNode("start-timeout-node", store.HashToken("token"), "start-timeout-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.DB().Create(&store.GPU{NodeID: node.ID, Index: 0, UUID: "start-timeout-gpu", Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 80 << 30}).Error; err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/start-timeout", Name: "start-timeout", WeightBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/start-timeout"}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "start-timeout", ModelID: model.ID, NodeID: node.ID, ServedName: "start-timeout", Engine: "vllm",
				Image: "vllm/vllm-openai:v0.11.0", State: "active", StartTimeoutSec: tc.deployment,
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			ctrl := New(st, hub, nil)
			if tc.harness > 0 {
				ctrl.SetStartTimeout(tc.harness)
			}
			if err := ctrl.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(instances) != 1 {
				t.Fatalf("instances=%+v err=%v", instances, err)
			}
			var desired proto.InstanceDesiredState
			if err := json.Unmarshal([]byte(instances[0].DesiredStateJSON), &desired); err != nil {
				t.Fatal(err)
			}
			if desired.StartTimeout != tc.want {
				t.Fatalf("instance start timeout = %d, want %d", desired.StartTimeout, tc.want)
			}
		})
	}
}
