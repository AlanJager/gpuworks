package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestKunlunMultiNodeEvaluateAndCreatePreserveGlobalTP(t *testing.T) {
	st, err := store.Open("file:kunlun_multinode_tp?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for rank, uuid := range []string{"p800-a", "p800-b"} {
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:     proto.SysInfo{Hostname: uuid, OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "kunlunxin", Framework: "xre"},
			Network: testManagement(fmt.Sprintf("192.0.2.%d", rank+10)),
			GPUs: []proto.GPUInfo{
				{Index: 0, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotalBytes: 96 << 30, Healthy: true},
				{Index: 1, Name: "P800 PCIe", Vendor: "kunlunxin", ArchID: "kl3", MemoryTotalBytes: 96 << 30, Healthy: true},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B", WeightBytes: 1519182438,
		Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"max_position_embeddings":40960}`})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := fmt.Sprintf(`{"name":"p800-multinode","model_id":%d,"engine":"sglang","target_vendor":"kunlunxin","image":"example/kunlun:sglang","nnodes":2,"node_uuids":["p800-a","p800-b"],"placement":{"tp":4,"pp":1}}`, model.ID)
	evaluate := func(body string) dto.EvaluateResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		var result dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("evaluate: %d %s, %v", rec.Code, rec.Body.String(), err)
		}
		return result
	}
	result := evaluate(body)
	if !result.OK || len(result.RankCommands) != 2 {
		t.Fatalf("multinode preview: %+v", result)
	}
	for rank, command := range result.RankCommands {
		argv := strings.Join(command.Command, " ")
		if command.NodeRank != rank || command.Headless != (rank == 1) || !strings.Contains(argv, "--tp-size 4") || !strings.Contains(argv, fmt.Sprintf("--nnodes 2 --node-rank %d", rank)) || !strings.Contains(argv, "--dist-init-addr 192.0.2.10:29500") {
			t.Fatalf("rank %d: %+v", rank, command)
		}
	}
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var dep dto.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &dep); err != nil {
		t.Fatal(err)
	}
	if dep.NNodes != 2 || len(dep.NodeUUIDs) != 2 || dep.MasterAddr != "192.0.2.10" {
		t.Fatalf("created topology: %+v", dep)
	}
	for _, bad := range []string{
		strings.Replace(body, `"pp":1`, `"pp":2`, 1),
		strings.Replace(body, `["p800-a","p800-b"]`, `["p800-a","p800-a"]`, 1),
	} {
		if result := evaluate(bad); result.OK {
			t.Fatalf("invalid topology accepted: %s", bad)
		}
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(bad)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid create: %d %s", rec.Code, rec.Body.String())
		}
	}
	if deps, err := st.ListDeployments(); err != nil || len(deps) != 1 {
		t.Fatalf("invalid topology persisted: %v %v", deps, err)
	}
}
