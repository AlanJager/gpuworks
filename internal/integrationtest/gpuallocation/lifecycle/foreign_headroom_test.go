package lifecycle_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// Real API, controller, SQLite ledger and agent; fake inventory and runtime.
// A 22 GiB card on which the agent reports a 10 GiB process the platform did
// not start. The headroom kept beside that process is the deployment
// policy's, and one setting decides three things together: what the GPU list
// calls free, what the deploy form's evaluation admits, and what a created
// deployment is placed against. Storing no headroom hands it back to all
// three; restoring it evicts nobody.
func TestForeignHeadroomSettingReachesTheGPUListAndAdmission(t *testing.T) {
	const (
		total   = int64(22528) << 20 // 22 GiB
		foreign = 10 * fixtures.GiB
	)
	env := testenv.New(t)
	node := env.AddNode("held", testenv.WithGPUs(1, testenv.GPUSpec{Model: "GeForce RTX 2080 Ti", VRAMMB: 22528}), testenv.WithDriverName("docker"))
	env.WaitNodeOnline(t, node.Name)
	storedNode, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{Source: "huggingface", SourceID: "allocation/headroom", Revision: "main", Name: "headroom", WeightBytes: 1 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: storedNode.ID, State: proto.MFPresent, Path: testenv.ModelDir(t, "headroom")}); err != nil {
		t.Fatal(err)
	}
	api := env.API(t)

	// The agent names the outside process on the card in its next report;
	// reconnecting sends one now instead of at the next minute.
	held := []proto.GPUInfo{{Index: 0, UUID: "GPU-held-0", Name: "GeForce RTX 2080 Ti", MemoryTotalBytes: total, Vendor: "nvidia", Healthy: true,
		ForeignProcesses: []proto.CardProcess{{PID: 3565129, Name: "VLLM::EngineCore", MemoryBytes: foreign}}}}
	node.Prober.SetGPUs(held)
	env.StopNode(t, node.Name)
	env.RestartNode(t, node.Name)
	env.WaitNodeOnline(t, node.Name)

	card := func() dto.PhysicalGPU {
		t.Helper()
		resp, err := api.Get("/api/v1/gpus")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var list dto.GPUListResponse
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil || resp.StatusCode != http.StatusOK || len(list.Items) != 1 {
			t.Fatalf("GET /gpus = %d items %d err %v", resp.StatusCode, len(list.Items), err)
		}
		return list.Items[0]
	}
	// The GPU list is a snapshot that a change rebuilds after its refresh
	// floor, so each expectation is waited for rather than read once.
	waitCard := func(what string, want func(dto.PhysicalGPU) bool) dto.PhysicalGPU {
		t.Helper()
		var got dto.PhysicalGPU
		if !env.EventuallyNoFail(40*time.Second, func() bool { got = card(); return want(got) }) {
			t.Fatalf("%s: card stayed at state %s allocated %d sleeping %d foreign %d available %d holders %+v",
				what, got.AllocationState, got.AllocatedBytes, got.SleepingBytes, got.ForeignBytes, got.AvailableBytes, got.ForeignProcesses)
		}
		return got
	}
	putPolicy := func(body string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPut, env.URL()+"/api/v1/settings/deployment", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := api.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT /settings/deployment %s = %d %s", body, resp.StatusCode, raw)
		}
	}
	// A 10 GiB slice claims 10.5 GiB: more than the 8 GiB the shipped
	// headroom leaves, less than the 12 GiB the process itself leaves.
	request := func(name string) []byte {
		return []byte(fmt.Sprintf(`{"name":%q,"served_name":%q,"model_id":%d,"node_uuid":%q,"placement_mode":"manual","placement":{"gpus":[0],"tp":1},"vram_limit_gib":10,"quick_params":{"max_model_len":256}}`,
			name, name, model.ID, node.UUID))
	}
	type verdict struct {
		OK      bool `json:"ok"`
		Message struct {
			Code   string         `json:"code"`
			Params map[string]any `json:"params"`
		} `json:"message"`
	}
	evaluate := func() verdict {
		t.Helper()
		resp, err := api.PostJSON("/api/v1/evaluate", request("slice"))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("evaluate = %d %s", resp.StatusCode, raw)
		}
		var out struct {
			Plan verdict `json:"plan"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("evaluate body %s: %v", raw, err)
		}
		return out.Plan
	}

	// Shipped policy: 5% of the card, at least 4 GiB.
	got := waitCard("the outside process is reported", func(g dto.PhysicalGPU) bool { return len(g.ForeignProcesses) == 1 && g.ForeignBytes == foreign })
	if got.AllocationState != "unusable" || got.Schedulable || got.SleepingBytes != 0 || got.AllocatedBytes != 0 || got.AvailableBytes != total-foreign-4*fixtures.GiB {
		t.Fatalf("shipped headroom: state %s schedulable %v allocated %d sleeping %d available %d, want unusable with %d free",
			got.AllocationState, got.Schedulable, got.AllocatedBytes, got.SleepingBytes, got.AvailableBytes, total-foreign-4*fixtures.GiB)
	}
	if v := evaluate(); v.OK || v.Message.Code != "no_fit" || v.Message.Params["foreign_gib"] != "10.0" || v.Message.Params["foreign_headroom_gib"] != "4.0" {
		t.Fatalf("shipped headroom, 10 GiB slice = ok %v %s %+v, want no_fit naming 10.0 GiB held and 4.0 GiB headroom", v.OK, v.Message.Code, v.Message.Params)
	}

	// No headroom: the card offers everything the process leaves.
	putPolicy(`{"foreign_headroom_pct":0,"foreign_headroom_min_bytes":0}`)
	got = waitCard("the headroom is handed back", func(g dto.PhysicalGPU) bool { return g.AvailableBytes == total-foreign })
	if got.ForeignBytes != foreign || got.AllocationState != "unusable" {
		t.Fatalf("no headroom: foreign %d state %s, want the holder's reading unchanged on a card that still takes no automatic work", got.ForeignBytes, got.AllocationState)
	}
	if got.MemoryTotalBytes-got.AllocatedBytes-got.SleepingBytes-got.ForeignBytes-got.AvailableBytes != 0 {
		t.Fatalf("no headroom: %d bytes of the card are left unexplained", got.MemoryTotalBytes-got.AllocatedBytes-got.SleepingBytes-got.ForeignBytes-got.AvailableBytes)
	}
	if v := evaluate(); !v.OK {
		t.Fatalf("no headroom, 10 GiB slice = %s %+v, want admitted", v.Message.Code, v.Message.Params)
	}
	resp, err := api.PostJSON("/api/v1/deployments", request("slice"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	var dep dto.Deployment
	if err := json.Unmarshal(raw, &dep); err != nil {
		t.Fatal(err)
	}
	waitRunning(t, env, dep.ID)
	claim := fixtures.NVIDIASliceClaim(10 * fixtures.GiB)
	got = waitCard("the slice is charged", func(g dto.PhysicalGPU) bool { return g.AllocatedBytes == claim })
	if got.AvailableBytes != total-foreign-claim || got.ForeignBytes != foreign || len(got.Occupants) != 1 {
		t.Fatalf("slice beside the process: available %d foreign %d occupants %d, want %d free beside both", got.AvailableBytes, got.ForeignBytes, len(got.Occupants), total-foreign-claim)
	}

	// Restoring the headroom takes the free figure away and evicts nobody.
	putPolicy(`{"foreign_headroom_pct":5,"foreign_headroom_min_bytes":4294967296}`)
	got = waitCard("the headroom is kept again", func(g dto.PhysicalGPU) bool { return g.AvailableBytes == 0 })
	if got.AllocatedBytes != claim || got.ForeignBytes != foreign {
		t.Fatalf("headroom restored: allocated %d foreign %d, want the slice and the holder as they were", got.AllocatedBytes, got.ForeignBytes)
	}
	rows, err := env.Store().InstancesForDeployment(dep.ID)
	if err != nil || len(rows) != 1 || rows[0].State != proto.InstRunning {
		t.Fatalf("headroom restored: instances %+v %v, want the slice still running", rows, err)
	}
	if v := evaluate(); v.OK {
		t.Fatalf("headroom restored, a second slice = admitted, want refused on a card with nothing left")
	}
}
