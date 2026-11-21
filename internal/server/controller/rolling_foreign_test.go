package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A partition whose card processes outside the platform took after it was
// placed is not taken down by a rolling restart while nobody enabled
// scheduling on the card: its replacement could not be placed, so the roll
// waits and names the card an operator may enable (ADR 0068). Enabled, the
// roll proceeds.
func TestRollingRestartKeepsAReplicaWhoseReplacementTheHeldCardWouldRefuse(t *testing.T) {
	for i, allow := range []bool{false, true} {
		t.Run(fmt.Sprintf("enabled=%v", allow), func(t *testing.T) {
			st, err := store.Open(fmt.Sprintf("file:rolling_foreign_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			node := foreignHeldH20(t, st, "h20-3e", proto.PartitionSafetyForeignTracked)
			model, err := st.UpsertModel(&store.Model{
				Source: "huggingface", SourceID: "fixture/rolling-foreign", Revision: "main", Name: "rolling-foreign", WeightBytes: 15 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/rolling-foreign"}); err != nil {
				t.Fatal(err)
			}
			other, err := createTestDeployment(t, st, &store.Deployment{Name: "whole-card", ModelID: model.ID, NodeID: node.ID, ServedName: "whole-card", Engine: "vllm", State: "active", Replicas: 1})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: "whole-card-1", DeploymentID: other.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[1]"}); err != nil {
				t.Fatal(err)
			}
			dep, err := createTestDeployment(t, st, &store.Deployment{
				Name: "beside", ModelID: model.ID, NodeID: node.ID, ServedName: "beside", Engine: "vllm", State: "active", Replicas: 1,
				Placement: `{"tp":1}`, QuickParams: `{"max_model_len":512}`,
				VRAMLimitBytes: 16 << 30, PartitionBackend: accelerator.BackendNVIDIALogical,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Placed while card 0 was free of outside processes.
			if err := st.DB().Model(&store.GPU{}).Where("node_id = ?", node.ID).Update("foreign_processes", "").Error; err != nil {
				t.Fatal(err)
			}
			hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
			c := New(st, hub, nil)
			c.SetDrainProbe(fakeDrainProbe{})
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			insts, err := st.InstancesForDeployment(dep.ID)
			if err != nil || len(insts) != 1 || insts[0].GPUIndexes != "[0]" {
				t.Fatalf("instances = %+v %v, want one on card 0", insts, err)
			}
			replica := insts[0].UUID
			_ = st.SetInstanceState(replica, proto.InstRunning, "")
			if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", replica).Update("created_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
				t.Fatal(err)
			}
			// A process outside the platform appears on the replica's card;
			// the operator has enabled scheduling on it or not, as the
			// case says.
			holdCard(t, st, node, 0)
			if allow {
				enableHeldCards(t, st, node.UUID)
			}
			if err := st.RequestRollingRestart(dep.ID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
				t.Fatal(err)
			}
			got, _ := st.InstanceByUUID(replica)
			if allow {
				if got.State != proto.InstDraining {
					t.Fatalf("enabled: replica %s, want draining", got.State)
				}
				return
			}
			if got.State != proto.InstRunning {
				t.Fatalf("not enabled: replica %s, want it kept running", got.State)
			}
			last, found, err := st.LatestScheduleEvent(dep.ID)
			if err != nil || !found {
				t.Fatalf("no refusal recorded: %v %v", found, err)
			}
			if data := fmt.Sprint(last.Data); !strings.Contains(data, scheduler.ReasonHeldOutsidePlatformNotAllowed) || strings.Contains(data, "setting") {
				t.Fatalf("recorded = %s, want %s naming no setting", data, scheduler.ReasonHeldOutsidePlatformNotAllowed)
			}
		})
	}
}
