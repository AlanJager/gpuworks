package lifecycle_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

func mustRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return req
}

func waitRunning(t *testing.T, env *testenv.Env, id uint) {
	t.Helper()
	env.Eventually(t, 30*time.Second, func() bool {
		rows, err := env.Store().InstancesForDeployment(id)
		return err == nil && len(rows) == 1 && rows[0].State == proto.InstRunning
	})
}

// Real API, controller, SQLite ledger and agent; fake inventory and runtime.
// Three simultaneous intents contend for one card. Cleanup failures must keep
// the old claim until the runtime confirms removal, then unblock the waiter.
func TestConcurrentDeploymentsRetainClaimsUntilCleanup(t *testing.T) {
	for _, partition := range []bool{false, true} {
		t.Run(fmt.Sprintf("partition-%v", partition), func(t *testing.T) {
			env := testenv.New(t)
			driver := testenv.NewFakeDriver("docker")
			env.AddNode("contention", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(driver))
			env.WaitNodeOnline(t, "contention")
			nodes, err := env.Store().ListNodes()
			if err != nil || len(nodes) != 1 {
				t.Fatalf("nodes=%+v err=%v", nodes, err)
			}
			model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "contention/model", Revision: "main", Name: "contention-model", Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30,
				Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: nodes[0].ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "contention")}); err != nil {
				t.Fatal(err)
			}
			api := env.API(t)
			limit, slots := 0, 1
			if partition {
				limit, slots = 8, 2
			}
			type createdResult struct {
				dep dto.Deployment
				err error
			}
			results := make(chan createdResult, 3)
			start := make(chan struct{})
			for i := 0; i < 3; i++ {
				go func() {
					<-start
					body := fmt.Sprintf(`{"name":"contender-%d","served_name":"contender-%d","model_id":%d,"node_uuid":%q,"placement_mode":"manual","placement":{"tp":1,"gpus":[0]},"vram_limit_gib":%d,"quick_params":{"max_model_len":256}}`, i, i, model.ID, nodes[0].UUID, limit)
					resp, err := api.PostJSON("/api/v1/deployments", []byte(body))
					if err != nil {
						results <- createdResult{err: err}
						return
					}
					defer resp.Body.Close()
					data, err := io.ReadAll(resp.Body)
					if err == nil && resp.StatusCode != http.StatusCreated {
						err = fmt.Errorf("create=%d %s", resp.StatusCode, data)
					}
					var dep dto.Deployment
					if err == nil {
						err = json.Unmarshal(data, &dep)
					}
					results <- createdResult{dep: dep, err: err}
				}()
			}
			close(start)
			ids := map[uint]bool{}
			for i := 0; i < 3; i++ {
				r := <-results
				if r.err != nil {
					t.Fatal(r.err)
				}
				ids[r.dep.ID] = true
			}
			var running []store.Instance
			env.Eventually(t, 30*time.Second, func() bool {
				rows, err := env.Store().ListInstances()
				if err != nil {
					return false
				}
				running = nil
				for _, row := range rows {
					if ids[row.DeploymentID] && row.State == proto.InstRunning {
						running = append(running, row)
					}
				}
				return len(running) == slots && len(rows) == slots
			})
			winner := running[0]
			waiting := map[uint]bool{}
			for id := range ids {
				waiting[id] = true
			}
			for _, row := range running {
				delete(waiting, row.DeploymentID)
			}
			var allowRemoval atomic.Bool
			var failedRemovals atomic.Int32
			driver.OnRemove(func(string) (error, bool) {
				if !allowRemoval.Load() {
					failedRemovals.Add(1)
					return errors.New("injected cleanup failure"), true
				}
				return nil, false
			})
			// Cleanup also succeeds if an assertion aborts the case.
			t.Cleanup(func() { allowRemoval.Store(true) })
			resp, err := api.PostJSON(fmt.Sprintf("/api/v1/deployments/%d/stop", winner.DeploymentID), nil)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != http.StatusOK {
				t.Fatalf("stop=%d %s err=%v", resp.StatusCode, data, err)
			}
			env.Eventually(t, 15*time.Second, func() bool {
				row, err := env.Store().InstanceByUUID(winner.UUID)
				return failedRemovals.Load() > 0 && err == nil && row.State == proto.InstRemoving
			})
			node, err := env.Store().NodeWithGPUs(nodes[0].UUID)
			if err != nil {
				t.Fatal(err)
			}
			ledger := planning.GPUAllocations(env.Store(), node)[0]
			wantBytes := int64(24 << 30)
			if partition {
				wantBytes = int64(slots) * fixtures.NVIDIASliceClaim(int64(limit)*fixtures.GiB)
			}
			if ledger.AllocatedBytes != wantBytes || len(ledger.Instances) != slots {
				t.Fatalf("failed cleanup released claim: %+v, want %d bytes / %d instances", ledger, wantBytes, slots)
			}
			rows, err := env.Store().ListInstances()
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if waiting[row.DeploymentID] {
					t.Fatalf("waiting deployment launched before cleanup: %+v", row)
				}
			}
			allowRemoval.Store(true)
			// Other deployments retry on the next periodic node report (one
			// minute), after the pool's 20-second settle backoff. Teardown's
			// own instance event reconciles only its owning deployment.
			// Allow two report periods, the backoff and loaded-runner slack.
			if !env.EventuallyNoFail(3*time.Minute, func() bool {
				rows, err := env.Store().ListInstances()
				if err != nil || len(rows) != slots {
					return false
				}
				resumed := false
				for _, row := range rows {
					if row.UUID == winner.UUID || row.State != proto.InstRunning || row.GPUIndexes != "[0]" {
						return false
					}
					resumed = resumed || waiting[row.DeploymentID]
				}
				return resumed
			}) {
				rows, err := env.Store().ListInstances()
				for _, row := range rows {
					t.Logf("instance=%s deployment=%d state=%s phase=%s message=%s", row.UUID, row.DeploymentID, row.State, row.Phase, row.Message)
				}
				t.Fatalf("waiting deployment did not resume after cleanup; list error=%v", err)
			}
			ledger = planning.GPUAllocations(env.Store(), node)[0]
			if ledger.AllocatedBytes != wantBytes || len(ledger.Instances) != slots {
				t.Fatalf("replacement overcommitted or leaked memory: %+v", ledger)
			}
		})
	}
}
