package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// suggestFixture is five one-card nodes: s-1..s-4 on the "ib" compute
// network, s-5 reachable only over the management network; s-4 cordoned. A
// two-node deployment runs its one replica on s-1 and s-2, whose cards it
// holds.
func suggestFixture(t *testing.T) (*apiHandlers, *store.Store, *store.Deployment) {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ib, err := st.CreateNetwork(&store.Network{Name: "ib", Type: netplan.TypeHostEthernet, CIDR: "10.96.0.0/13", Roles: store.EncodeStringList([]string{netplan.RoleCompute})})
	if err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*store.Node{}
	for i := 1; i <= 5; i++ {
		uuid := fmt.Sprintf("s-%d", i)
		if _, err := st.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		inv := testManagement(fmt.Sprintf("172.25.0.%d", i))
		if i <= 4 {
			inv.Interfaces = append(inv.Interfaces, proto.NetInterface{Name: "ib0", Addrs: []string{fmt.Sprintf("10.96.0.%d/13", i)}})
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:     proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
			Network: inv,
			GPUs:    []proto.GPUInfo{{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
		}); err != nil {
			t.Fatal(err)
		}
		node, _ := st.NodeByUUID(uuid)
		nodes[uuid] = node
		if i <= 4 {
			if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: ib.ID, NodeID: node.ID}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := st.SetNodeScheduling("s-4", store.NodeCordoned); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/suggest", Revision: "main", Name: "suggest", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	nodeIDs, _ := json.Marshal([]uint{nodes["s-1"].ID, nodes["s-2"].ID})
	dep, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "multi", ModelID: model.ID, NodeID: nodes["s-1"].ID, NodeIDs: string(nodeIDs),
		Engine: "vllm", Image: "example/vllm:test", NNodes: 2, Replicas: 1, Placement: `{"tp":2}`, State: store.DeployActive, DistGroup: "g"}, map[string][]string{"gpuworks.ai/vendor": {"nvidia"}})
	if err != nil {
		t.Fatal(err)
	}
	for rank, uuid := range []string{"s-1", "s-2"} {
		if _, err := st.CreateInstance(&store.Instance{UUID: "multi-" + uuid, DeploymentID: dep.ID, NodeID: nodes[uuid].ID, State: proto.InstRunning,
			GPUIndexes: "[0]", NNodes: 2, NodeRank: rank, Headless: rank > 0, DistGroup: "g", DistAttempt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return &apiHandlers{d: Deps{Store: st}}, st, dep
}

func postSuggest(t *testing.T, h *apiHandlers, id uint, body string) (int, string, dto.UnitSuggestion) {
	t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", fmt.Sprint(id))
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/deployments/%d/units/suggest", id), strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.suggestUnit(rec, req)
	var got dto.UnitSuggestion
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	return rec.Code, rec.Body.String(), got
}

const listedReplica = `{"units":[{"node_uuid":"s-1","followers":[{"node_uuid":"s-2"}]}]}`

// With the running replica's nodes busy and s-4 cordoned, the compute
// network has one free node and the management-only node is a group of its
// own: nothing is suggested, and the counts say why.
func TestSuggestUnitSaysWhyNothingFits(t *testing.T) {
	h, _, dep := suggestFixture(t)
	status, body, got := postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit != nil || got.Reason == nil || got.Reason.Code != "unit_none" {
		t.Fatalf("suggest = %d %s", status, body)
	}
	for _, want := range []string{`"ranks":2`, `"per_rank":1`, `"nodes":5`, `"cordoned":1`, `"busy":2`, `"largest":1`, `"unmet":0`, `"offline":0`, `"no_network":0`} {
		if !strings.Contains(body, want) {
			t.Errorf("reason lacks %s: %s", want, body)
		}
	}
}

// Uncordoned, s-3 and s-4 share the compute network and one architecture:
// they are suggested, each rank trial-planned, with the network named. A
// listed replica the deployment does not have yet claims its nodes' cards.
func TestSuggestUnitProposesAGroupOnOneNetwork(t *testing.T) {
	h, st, dep := suggestFixture(t)
	if _, err := st.SetNodeScheduling("s-4", store.NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	status, body, got := postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit == nil {
		t.Fatalf("suggest = %d %s", status, body)
	}
	const want = `{"unit":{"node_uuid":"s-3","followers":[{"node_uuid":"s-4"}]},"network":{"id":1,"name":"ib"},"arch":"nvidia/ampere",` +
		`"nodes":[{"uuid":"s-3","name":"s-3","free_gpus":1,"model":"absent"},{"uuid":"s-4","name":"s-4","free_gpus":1,"model":"absent"}]}`
	if strings.TrimSpace(body) != want {
		t.Fatalf("suggestion = %s\nwant %s", body, want)
	}

	// The operator has already listed s-3 and s-4 for a new replica: their
	// cards are spoken for, so nothing else is left to suggest.
	status, body, got = postSuggest(t, h, dep.ID, `{"units":[{"node_uuid":"s-1","followers":[{"node_uuid":"s-2"}]},{"node_uuid":"s-3","followers":[{"node_uuid":"s-4"}]}]}`)
	if status != http.StatusOK || got.Unit != nil || got.Reason == nil || got.Reason.Code != "unit_none" || !strings.Contains(body, `"busy":4`) {
		t.Fatalf("with s-3 and s-4 claimed = %d %s", status, body)
	}
}

// A replica of a stopped deployment holds no cards now but takes them when
// the deployment starts again, so they are not offered to another replica
// (seen on three one-card 4090 hosts: the stored replica's own two nodes
// came back as the suggestion). A replica still running holds its cards in
// the ledger and is not counted twice.
func TestSuggestUnitLeavesTheCardsOfAStoppedReplica(t *testing.T) {
	h, st, dep := suggestFixture(t)
	if _, err := st.SetNodeScheduling("s-4", store.NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Instance{}).Where("deployment_id = ?", dep.ID).Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	status, body, got := postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit == nil || got.Unit.NodeUUID != "s-3" || len(got.Unit.Followers) != 1 || got.Unit.Followers[0].NodeUUID != "s-4" {
		t.Fatalf("suggest for a stopped deployment = %d %s, want s-3 and s-4 with s-1 and s-2 left to the stored replica", status, body)
	}

	// One rank still running: only the stopped rank's node is claimed.
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "multi-s-1").Update("state", proto.InstRunning).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Node{}).Where("uuid IN ?", []string{"s-3", "s-4"}).Update("scheduling", store.NodeCordoned).Error; err != nil {
		t.Fatal(err)
	}
	status, body, got = postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit != nil || got.Reason == nil || !strings.Contains(body, `"busy":2`) {
		t.Fatalf("with s-1 running and s-2 stopped = %d %s, want both busy and nothing suggested", status, body)
	}
}

// Only a multi-node deployment lists replicas as units; malformed requests
// and unknown deployments are refused.
func TestSuggestUnitRefusals(t *testing.T) {
	h, st, dep := suggestFixture(t)
	single, err := st.CreateDeployment(&store.Deployment{Name: "single", ModelID: dep.ModelID, NodeID: dep.NodeID, Engine: "vllm", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if status, body, _ := postSuggest(t, h, single.ID, `{"units":[]}`); status != http.StatusBadRequest || !strings.Contains(body, ErrorReplicasBodyShape) {
		t.Errorf("single-node = %d %s", status, body)
	}
	if status, body, _ := postSuggest(t, h, dep.ID, `{"units":[],"extra":1}`); status != http.StatusBadRequest || !strings.Contains(body, "unknown field") {
		t.Errorf("unknown field = %d %s", status, body)
	}
	if status, _, _ := postSuggest(t, h, 9999, `{"units":[]}`); status != http.StatusNotFound {
		t.Errorf("unknown deployment = %d", status)
	}
	rec := httptest.NewRecorder()
	Router(Deps{Store: st}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/deployments/%d/units/suggest", dep.ID), strings.NewReader(`{"units":[]}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated = %d", rec.Code)
	}
}

// A group whose ranks the scheduler refuses - a model too large for the
// cards - is not suggested; the first refusal is what the operator reads.
func TestSuggestUnitReportsTheTrialRefusal(t *testing.T) {
	h, st, dep := suggestFixture(t)
	if _, err := st.SetNodeScheduling("s-4", store.NodeSchedulable); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Model{}).Where("id = ?", dep.ModelID).Update("weight_bytes", int64(400)<<30).Error; err != nil {
		t.Fatal(err)
	}
	status, body, got := postSuggest(t, h, dep.ID, listedReplica)
	if status != http.StatusOK || got.Unit != nil || got.Reason == nil || got.Reason.Code != "unit_no_fit" ||
		!strings.Contains(body, `"node":"s-3"`) || !strings.Contains(body, `"first_reason":{"code":"no_fit"`) {
		t.Fatalf("suggest = %d %s", status, body)
	}
}
