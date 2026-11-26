package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// twoNodesFixture adds a second node to twoCardFixture's cluster: one card
// whose hardware index is 0, so a placement naming card 1 names a card only
// the first node reports.
func twoNodesFixture(t *testing.T) (*apiHandlers, *store.Store, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "_", "-")+"-nodes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	card := func(index int) proto.GPUInfo {
		return proto.GPUInfo{Index: index, Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	for uuid, cards := range map[string][]proto.GPUInfo{"two-card": {card(0), card(1)}, "one-card": {card(0)}} {
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: cards,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/two-nodes", Revision: "main", Name: "two-nodes", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, st, model.ID
}

// pinnedToCardOne creates a deployment pinned to card 1 of the two-card node.
func pinnedToCardOne(t *testing.T, h *apiHandlers, modelID uint) dto.Deployment {
	t.Helper()
	code, resp := createOnTwoCards(t, h, fmt.Sprintf(`{"name":"moved","model_id":%d,"engine":"vllm","node_uuid":"two-card","image":"example/engine:test","placement":{"gpus":[1],"tp":1},"quick_params":{}}`, modelID))
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, resp)
	}
	var dep dto.Deployment
	if err := json.Unmarshal([]byte(resp), &dep); err != nil {
		t.Fatal(err)
	}
	return dep
}

// GWKS-522: a deployment pinned to card 1 of a two-card node is edited onto a
// one-card node while the form still carries the card choice made for the old
// node. An index names a card on one machine, so the edit writes the
// deployment without it and the scheduler picks the new node's cards, instead
// of the save failing with "has no GPU with index 1".
func TestEditOntoAnotherNodeDropsTheOldNodesCards(t *testing.T) {
	h, st, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	body := editBody(t, h, dep.ID,
		`"node_uuid":"two-card"`, `"node_uuid":"one-card"`,
		`"placement":{"gpus":[1],"tp":1}`, `"placement":{"gpus":[1],"tp":1}`)
	status, resp := putEdit(t, h, dep.ID, body, false)
	if status != http.StatusOK {
		t.Fatalf("edit = %d %s", status, resp)
	}
	row, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Placement, `"gpus"`) {
		t.Fatalf("stored placement keeps the old node's card: %s", row.Placement)
	}
	if row.NodeID == 0 || row.Placement != `{"tp":1}` {
		t.Fatalf("stored deployment node=%d placement=%s", row.NodeID, row.Placement)
	}
}

// The same edit previewed (the form sends the deployment it edits): the
// evaluation plans the new node with the cards it has instead of failing on
// the old node's index, and names no "deployment 0".
func TestEvaluateOnAnotherNodeDropsTheOldNodesCards(t *testing.T) {
	h, _, modelID := twoNodesFixture(t)
	dep := pinnedToCardOne(t, h, modelID)
	body := fmt.Sprintf(`{"name":"moved","model_id":%d,"engine":"vllm","node_uuid":"one-card","image":"example/engine:test","placement":{"gpus":[1],"tp":1},"quick_params":{},"deployment_id":%d}`, modelID, dep.ID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body)
	}
	var resp dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("evaluate refused a placement the node can run: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "deployment 0") {
		t.Fatalf("preview names deployment 0: %s", rec.Body)
	}
	if len(resp.Errors) > 0 {
		t.Fatalf("evaluate errors = %+v", resp.Errors)
	}
	// The plan ran: the preview picked the new node's own card, not the
	// index the request carried over from the old one.
	var wire struct {
		Plan struct {
			Plan struct {
				GPUs []int `json:"gpus"`
			} `json:"plan"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wire.Plan.Plan.GPUs, []int{0}) {
		t.Fatalf("planned gpus = %v, want [0] (the one-card node's only card)", wire.Plan.Plan.GPUs)
	}
}
