// Package fixtures builds persisted business inputs shared by allocation tests.
// It does not run an agent, contact a model registry, or emulate scheduling.
package fixtures

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const GiB int64 = 1 << 30

// World owns a temporary store, a two-card NVIDIA node and a local model.
// The reported docker driver is a capability fact, never an invoked runtime.
type World struct {
	Store *store.Store
	Node  *store.Node
	Model *store.Model
}

func NewWorld(t *testing.T) World {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "allocation.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	node, err := st.RegisterNode("allocation-node", store.HashToken("allocation-token"), "allocation-node")
	if err != nil {
		t.Fatal(err)
	}
	status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: node.Name, Arch: "amd64", OS: "linux", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4", RuntimeDriver: "docker",
		PartitionSafetyProtocol: proto.PartitionSafetyForeignTracked,
		PartitionAssets:         []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}}}}
	for index := 0; index < 2; index++ {
		status.GPUs = append(status.GPUs, proto.GPUInfo{Index: index, UUID: fmt.Sprintf("allocation-gpu-%d", index), Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 64 * GiB, Healthy: true})
		status.Sys.PartitionProbes = append(status.Sys.PartitionProbes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
	}
	if err := st.UpdateNodeStatus(node.UUID, status); err != nil {
		t.Fatal(err)
	}
	node, err = st.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "allocation/model", Revision: "main", Name: "allocation-model", Arch: "Qwen3ForCausalLM", WeightBytes: 8 * GiB,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return World{Store: st, Node: node, Model: model}
}

// Deployment creates a ledger owner. Scheduling is exercised by the caller.
func (w World) Deployment(t *testing.T, name string, limit int64) *store.Deployment {
	t.Helper()
	dep := &store.Deployment{Name: name, ServedName: name, ModelID: w.Model.ID, NodeID: w.Node.ID, Engine: "vllm", State: store.DeployActive, Sleepable: true, VRAMLimitBytes: limit}
	if limit > 0 {
		dep.PartitionBackend = accelerator.BackendNVIDIALogical
	}
	created, err := w.Store.CreateDeployment(dep)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// Instance preserves the desired quota in the same protocol object as a launch.
func (w World) Instance(t *testing.T, dep *store.Deployment, uuid, state, phase string, index int, claim int64) *store.Instance {
	t.Helper()
	spec := proto.InstanceDesiredState{UUID: uuid, Engine: "vllm", DesiredState: "running", Sleepable: true,
		VRAMLimitBytes: dep.VRAMLimitBytes, PartitionBackend: dep.PartitionBackend, MemoryClaimBytes: claim}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	in, err := w.Store.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: w.Node.ID, State: state, Phase: phase, GPUIndexes: fmt.Sprintf("[%d]", index), NNodes: 1, DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return in
}
