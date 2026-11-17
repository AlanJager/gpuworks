// Package fixtures builds persisted business inputs shared by allocation tests.
// It does not run an agent, contact a model registry, or emulate scheduling.
package fixtures

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const GiB int64 = 1 << 30

var worlds atomic.Int64

// World owns a temporary store, a two-card NVIDIA node and a local model.
// The reported docker driver is a capability fact, never an invoked runtime.
type World struct {
	Store *store.Store
	Node  *store.Node
	Model *store.Model
}

func NewWorld(t *testing.T) World {
	t.Helper()
	// In memory, as the store's own tests do: a file database makes every
	// matrix row pay for disk-backed migrations.
	st, err := store.Open(fmt.Sprintf("file:allocation-%d?mode=memory&cache=shared", worlds.Add(1)), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "allocation/model", Revision: "main", Name: "allocation-model", Arch: "Qwen3ForCausalLM", WeightBytes: 8 * GiB,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	w := World{Store: st, Model: model}
	w.Node = w.AddNode(t, "allocation-node", 2)
	return w
}

// AddNode reports another node and a complete local model copy in this store.
func (w World) AddNode(t *testing.T, name string, cards int) *store.Node {
	t.Helper()
	node, err := w.Store.RegisterNode(name, store.HashToken(name), name)
	if err != nil {
		t.Fatal(err)
	}
	status := proto.NodeStatus{Sys: proto.SysInfo{Hostname: name, Arch: "amd64", OS: "linux", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4", RuntimeDriver: "docker",
		PartitionSafetyProtocol: proto.PartitionSafetyForeignTracked,
		PartitionAssets:         []proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}}}}
	for index := 0; index < cards; index++ {
		status.GPUs = append(status.GPUs, proto.GPUInfo{Index: index, UUID: fmt.Sprintf("%s-gpu-%d", name, index), Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 64 * GiB, Healthy: true})
		status.Sys.PartitionProbes = append(status.Sys.PartitionProbes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
	}
	if err := w.Store.UpdateNodeStatus(node.UUID, status); err != nil {
		t.Fatal(err)
	}
	node, err = w.Store.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Store.UpsertModelFile(&store.ModelFile{ModelID: w.Model.ID, NodeID: node.ID, State: proto.MFPresent, Path: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	return node
}

// Deployment creates a ledger owner. Scheduling is exercised by the caller.
func (w World) Deployment(t *testing.T, name string, limit int64) *store.Deployment {
	t.Helper()
	dep := &store.Deployment{Name: name, ServedName: name, ModelID: w.Model.ID, NodeID: w.Node.ID, Engine: engines.VLLM, State: store.DeployActive, Sleepable: true, VRAMLimitBytes: limit}
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
	spec := proto.InstanceDesiredState{UUID: uuid, Engine: engines.VLLM, DesiredState: "running", Sleepable: true,
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
