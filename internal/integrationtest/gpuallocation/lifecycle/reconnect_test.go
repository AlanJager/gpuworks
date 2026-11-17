package lifecycle_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func TestAgentReconnectPreservesWholeAndPartitionClaims(t *testing.T) {
	for _, limit := range []int{0, 16} {
		t.Run(fmt.Sprintf("quota-%d", limit), func(t *testing.T) {
			env := testenv.New(t)
			node := env.AddNode("reconnect", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriverName("docker"))
			env.WaitNodeOnline(t, node.Name)
			storedNode, err := env.Store().NodeWithGPUs(node.UUID)
			if err != nil {
				t.Fatal(err)
			}
			model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "allocation/reconnect", Revision: "main", Name: "reconnect", WeightBytes: 1 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: storedNode.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "reconnect")}); err != nil {
				t.Fatal(err)
			}
			var runs, inspections atomic.Int32
			node.Driver.AfterRun(func(runtime.WorkloadSpec) { runs.Add(1) })
			node.Driver.AfterStatus(func(string, runtime.Status) { inspections.Add(1) })
			body := fmt.Sprintf(`{"name":"owner","served_name":"owner","model_id":%d,"node_uuid":%q,"placement_mode":"manual","placement":{"gpus":[0],"tp":1},"vram_limit_gib":%d,"quick_params":{"max_model_len":256}}`, model.ID, node.UUID, limit)
			resp, err := env.API(t).PostJSON("/api/v1/deployments", []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusCreated {
				t.Fatalf("create=%d %s err=%v", resp.StatusCode, data, err)
			}
			var dep dto.Deployment
			if err := json.Unmarshal(data, &dep); err != nil {
				t.Fatal(err)
			}
			waitRunning(t, env, dep.ID)
			rows, err := env.Store().InstancesForDeployment(dep.ID)
			if err != nil || len(rows) != 1 {
				t.Fatalf("instances=%+v %v", rows, err)
			}
			original := rows[0].UUID
			want := int64(24 << 30)
			if limit > 0 {
				want = fixtures.NVIDIASliceClaim(int64(limit) * fixtures.GiB)
			}
			assertClaim := func() {
				t.Helper()
				claim := planning.GPUAllocations(env.Store(), storedNode)[0]
				if claim.AllocatedBytes != want || len(claim.Instances) != 1 || claim.Instances[0].UUID != original {
					t.Fatalf("reconnect released or duplicated a claim: %+v, want %d for %s", claim, want, original)
				}
			}
			assertClaim()
			env.StopNode(t, node.Name)
			assertClaim() // Losing the transport does not confirm runtime removal.
			before := inspections.Load()
			env.RestartNode(t, node.Name)
			env.WaitNodeOnline(t, node.Name)
			env.Eventually(t, 30*time.Second, func() bool {
				row, err := env.Store().InstanceByUUID(original)
				return err == nil && row.State == proto.InstRunning && row.UpdatedAt.After(rows[0].UpdatedAt) && inspections.Load() > before
			})
			assertClaim()
			if runs.Load() != 1 {
				t.Fatalf("adoption launched another workload: %d runs", runs.Load())
			}
		})
	}
}
