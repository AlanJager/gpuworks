package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment stored before manual scripts were held to the assigned GPUs
// (GWKS-415: two cards assigned, the script states one) still starts: the
// trial the controller runs on a pool node before it launches reads the
// stored row as the launch does. A request that is being written is where the
// mismatch is refused.
func TestReconcileStartsAStoredDeploymentWhoseManualScriptStatesFewerCards(t *testing.T) {
	for _, tc := range []struct {
		engine string
		script string
	}{
		{engine: "vllm", script: "exec vllm serve /model --tensor-parallel-size 1 --port 30000"},
		{engine: "sglang", script: "exec python -m sglang.launch_server --model-path /model --tp-size 1 --host 127.0.0.1 --port 30000"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:controller_stored_manual_script_%s?mode=memory&cache=shared", tc.engine), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			name := "stored-script-" + tc.engine
			node, err := st.RegisterNode(name, store.HashToken(name), name)
			if err != nil {
				t.Fatal(err)
			}
			for index := 0; index < 2; index++ {
				if err := st.DB().Create(&store.GPU{
					NodeID: node.ID, Index: index, UUID: fmt.Sprintf("%s-gpu-%d", name, index),
					Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30,
				}).Error; err != nil {
					t.Fatal(err)
				}
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/" + name, Revision: "main", Name: name, WeightBytes: 1 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/" + name}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: name, ModelID: model.ID, NodeID: node.ID, ServedName: name, Engine: tc.engine, State: "active",
				Image: "example/engine:test", Placement: `{"tp":2,"gpus":[0,1]}`, QuickParams: `{}`,
				ContainerOverrides: fmt.Sprintf(`{"command_mode":"manual","cmd_display":%q}`, tc.script),
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			updates := hub.updates[node.UUID]
			if len(updates) != 1 || len(updates[0].Instances) != 1 {
				t.Fatalf("state updates=%+v, want one instance push", updates)
			}
			spec := updates[0].Instances[0]
			if len(spec.GPUIndexes) != 2 || spec.GPUIndexes[0] != 0 || spec.GPUIndexes[1] != 1 {
				t.Fatalf("launched on GPU indexes %v, want [0 1]", spec.GPUIndexes)
			}
			if len(spec.Argv) != 2 || !strings.Contains(spec.Argv[1], tc.script) {
				t.Fatalf("argv = %q, want the stored script as written", spec.Argv)
			}
		})
	}
}
