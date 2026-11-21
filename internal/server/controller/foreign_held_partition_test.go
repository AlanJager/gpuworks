package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// foreignHeldH20 is the node of flitai-io/gpuworks#20: two H20-3e cards of
// 143771 MiB, GPU 0 held by a GPUStack container's python3 (63982 MiB),
// nvidia-dgpu ready on both, its agent at the given partition safety level.
func foreignHeldH20(t *testing.T, st *store.Store, name string, safety int) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(name, store.HashToken(name), name)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := json.Marshal([]proto.CardProcess{{PID: 41235, Name: "python3", MemoryBytes: 67089334272}})
	for index := range 2 {
		card := &store.GPU{NodeID: node.ID, Index: index, UUID: name + "-gpu-" + string(rune('0'+index)), Name: "NVIDIA H20-3e",
			Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotal: 143771 << 20}
		if index == 0 {
			card.ForeignProcesses = string(held)
		}
		if err := st.DB().Create(card).Error; err != nil {
			t.Fatal(err)
		}
	}
	var probes []proto.PartitionProbeResult
	for index := range 2 {
		probes = append(probes, proto.PartitionProbeResult{Backend: accelerator.BackendNVIDIALogical, GPUIndex: &index, Status: proto.PartitionProbePassed})
	}
	assets, _ := json.Marshal([]proto.AssetReport{{Backend: accelerator.BackendNVIDIALogical, Root: "/assets", Files: []string{"libvgpu.so"}}})
	probeJSON, _ := json.Marshal(probes)
	if err := st.DB().Model(&store.Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"gpu_vendor": "nvidia", "arch": "amd64", "os": "linux", "runtime_driver": "docker",
		"partition_assets": string(assets), "partition_probes": string(probeJSON), "partition_safety_protocol": safety,
	}).Error; err != nil {
		t.Fatal(err)
	}
	reloaded, err := st.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	return reloaded
}

// enableHeldCards records an operator's decision to enable scheduling on
// every card of the node that processes outside the platform hold, the way
// the card's own action does (store.SetCardScheduling).
func enableHeldCards(t *testing.T, st *store.Store, nodeUUID string) {
	t.Helper()
	node, err := st.NodeWithGPUs(nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	enabled := 0
	for _, g := range node.GPUs {
		if !g.HeldOutsidePlatform() {
			continue
		}
		if _, err := st.SetCardScheduling(nodeUUID, g.Ident, store.CardSchedulingSchedulable); err != nil {
			t.Fatal(err)
		}
		enabled++
	}
	if enabled == 0 {
		t.Fatalf("node %s has no held card to enable", nodeUUID)
	}
}

// holdCard reports a process outside the platform on one of the node's
// cards, as a later status report would.
func holdCard(t *testing.T, st *store.Store, node *store.Node, index int) {
	t.Helper()
	held, _ := json.Marshal([]proto.CardProcess{{PID: 41235, Name: "python3", MemoryBytes: 67089334272}})
	if err := st.DB().Model(&store.GPU{}).Where("node_id = ? AND \"index\" = ?", node.ID, index).Update("foreign_processes", string(held)).Error; err != nil {
		t.Fatal(err)
	}
}

// The reported deployment - manual placement, pinned to GPU 0, TP 1, a 50 GiB
// nvidia-dgpu limit - starts beside the foreign process once the agent keeps
// reporting it: an operator who picked the card consented to sharing it. An
// automatic placement does so only on a card an operator enabled scheduling
// on, and otherwise waits naming the card. With an agent that would forget
// the holder, it waits and says the card is held outside the platform rather
// than "no free GPU" - unless the operator enabled the card, whose decision
// stands whatever the platform judged (store.GPU.CanTakeWork).
func TestTheReportedPinnedPartitionReplicaStartsBesideTheForeignProcess(t *testing.T) {
	for i, tc := range []struct {
		name   string
		safety int
		// automatic leaves the placement to the scheduler with GPU 1 taken
		// whole by another deployment: the shape that answered no_free_gpu.
		automatic bool
		// enable is the operator enabling scheduling on the held card.
		enable  bool
		starts  bool
		refusal string
	}{
		{"pinned, agent keeps foreign processes reported", proto.PartitionSafetyForeignTracked, false, false, true, ""},
		{"automatic, the other card taken, card enabled", proto.PartitionSafetyForeignTracked, true, true, true, ""},
		{"automatic, the other card taken, card not enabled", proto.PartitionSafetyForeignTracked, true, false, false, scheduler.ReasonHeldOutsidePlatformNotAllowed},
		{"pinned, agent before that", proto.PartitionSafetyOwnership, false, false, false, scheduler.ReasonGPUsHeldOutsidePlatform},
		{"automatic, agent before that", proto.PartitionSafetyOwnership, true, false, false, ""},
		{"automatic, agent before that, card enabled", proto.PartitionSafetyOwnership, true, true, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:foreign_held_partition_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node := foreignHeldH20(t, st, "h20-3e", tc.safety)
			if tc.enable {
				enableHeldCards(t, st, node.UUID)
			}
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/foreign-held", Revision: "main", Name: "foreign-held", WeightBytes: 15 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/foreign-held"}); err != nil {
				t.Fatal(err)
			}
			placement := `{"tp":1,"gpus":[0]}`
			if tc.automatic {
				placement = `{"tp":1}`
				other, err := createTestDeployment(t, st, &store.Deployment{Name: "whole-card", ModelID: model.ID, NodeID: node.ID, ServedName: "whole-card", Engine: "vllm", State: "active", Replicas: 1})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.CreateInstance(&store.Instance{UUID: "whole-card-1", DeploymentID: other.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
					t.Fatal(err)
				}
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "beside-gpustack", ModelID: model.ID, NodeID: node.ID, ServedName: "beside-gpustack", Engine: "vllm", State: "active",
				Placement: placement, QuickParams: `{"max_model_len":512}`,
				VRAMLimitBytes: 50 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
			})
			if err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			c := New(st, hub, nil)
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			instances, err := st.InstancesForDeployment(dep.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.starts {
				if len(instances) != 0 {
					t.Fatalf("instances = %+v, want none beside a holder an old agent would forget", instances)
				}
				last, found, err := st.LatestScheduleEvent(dep.ID)
				if err != nil || !found {
					t.Fatalf("no refusal recorded: found=%v err=%v", found, err)
				}
				data := fmt.Sprint(last.Data)
				if tc.refusal != "" && (!strings.Contains(data, tc.refusal) || strings.Contains(data, scheduler.ReasonNoFreeGPU)) {
					t.Fatalf("refusal = %s, want it to name %s, not %s", data, tc.refusal, scheduler.ReasonNoFreeGPU)
				}
				if tc.refusal == scheduler.ReasonHeldOutsidePlatformNotAllowed && (!strings.Contains(data, `"gpus":[0]`) || !strings.Contains(data, `"node":"h20-3e"`) || strings.Contains(data, "setting")) {
					t.Fatalf("refusal = %s, want it to name card 0 of the node and no setting", data)
				}
				return
			}
			if len(instances) != 1 || instances[0].GPUIndexes != "[0]" {
				t.Fatalf("instances = %+v, want one on GPU 0", instances)
			}
			updates := hub.updates[node.UUID]
			if len(updates) != 1 || len(updates[0].Instances) != 1 || updates[0].Instances[0].VRAMLimitBytes != 50<<30 {
				t.Fatalf("state updates = %+v, want the 50 GiB partition pushed", updates)
			}
		})
	}
}
