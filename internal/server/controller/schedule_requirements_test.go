package controller

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The controller rechecks a deployment's node requirements before every
// launch, against the node's written labels and the labels the platform
// generates from its facts. A node that stopped satisfying them - a label
// edited, a requirement on hardware it lacks, a network it is not on - gets
// no instance and the reason is recorded; one that satisfies them launches,
// which is what keeps the rejections from passing vacuously.
func TestReconcileDeploymentRechecksNodeRequirements(t *testing.T) {
	for name, tc := range map[string]struct {
		reqs   map[string][]string
		launch bool
		detail string
	}{
		"written label edited after creation": {map[string][]string{"pool": {"inference"}}, false, "pool ∈ {inference}"},
		"another GPU model":                   {map[string][]string{"gpuworks.ai/gpu-model": {"H100"}}, false, "gpuworks.ai/gpu-model ∈ {H100}"},
		"a network the node is not on":        {map[string][]string{"gpuworks.ai/compute-network": {"7"}}, false, "gpuworks.ai/compute-network ∈ {7}"},
		"another vendor":                      {map[string][]string{"gpuworks.ai/vendor": {"ascend"}}, false, "gpuworks.ai/vendor ∈ {ascend}"},
		"the current written label":           {map[string][]string{"pool": {"other"}}, true, ""},
		"its own GPU model, among others":     {map[string][]string{"gpuworks.ai/gpu-model": {"H100", "A100-SXM4-80GB"}}, true, ""},
		"its own vendor and node":             {map[string][]string{"gpuworks.ai/vendor": {"nvidia"}, "gpuworks.ai/node": {"requirement-node"}}, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:controller_requirements_"+strings.ReplaceAll(name, " ", "_")+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node, err := st.RegisterNode("requirement-node", store.HashToken("token"), "requirement-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
				Sys: proto.SysInfo{Hostname: "requirement-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
				GPUs: []proto.GPUInfo{{Index: 0, UUID: "requirement-a100", Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia",
					ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
			}); err != nil {
				t.Fatal(err)
			}
			// The labels are edited after the deployment was created with
			// requirements that used to match.
			if _, err := st.SetNodeLabels(node.UUID, map[string]string{"pool": "other"}); err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/requirements", Revision: "main", Name: "requirements",
				WeightBytes: 1 << 30, Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/requirements"}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeploymentWithRequirements(t, st, &store.Deployment{
				Name: "requirements", ModelID: model.ID, NodeID: node.ID, ServedName: "requirements", Engine: "vllm",
				Placement: `{"tp":1,"gpus":[0]}`, QuickParams: `{"max_model_len":512}`,
			}, tc.reqs)
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.launch {
				if len(instances) != 1 {
					t.Fatalf("instances = %+v, want the satisfied requirements to launch one", instances)
				}
				return
			}
			if len(instances) != 0 || len(hub.updates[node.UUID]) != 0 {
				t.Fatalf("an excluded node got instances=%+v updates=%+v", instances, hub.updates[node.UUID])
			}
			event, found, err := st.LatestScheduleEvent(dep.ID)
			if err != nil || !found || !strings.Contains(event.Data, "pool_empty") || !strings.Contains(event.Data, tc.detail) {
				t.Fatalf("schedule event=%+v found=%v err=%v, want %q", event, found, err, tc.detail)
			}
		})
	}
}
