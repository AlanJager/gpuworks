package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentPagePDReplicaCountsAndSort(t *testing.T) {
	h, st, created := createPDForOps(t)
	nodeA, err := st.NodeByUUID("pd-a")
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := st.CreateDeployment(&store.Deployment{Name: "ordinary", ModelID: created.ModelID, NodeID: nodeA.ID, Replicas: 2, State: store.DeployActive, Engine: "vllm"})
	if err != nil {
		t.Fatal(err)
	}
	check := func(ready int) {
		t.Helper()
		for _, query := range []string{"sort=replicas", "sort=-replicas"} {
			rec := httptest.NewRecorder()
			h.listDeployments(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments?page=1&page_size=10&"+query, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
			}
			var wire struct {
				Items []map[string]json.RawMessage `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			page := decodeDeploymentPage(t, rec.Body.Bytes())
			if len(page.Items) != 2 {
				t.Fatalf("items = %+v", page.Items)
			}
			pdIndex := 1
			if query == "sort=-replicas" {
				pdIndex = 0
			}
			pd, normal := page.Items[pdIndex], page.Items[1-pdIndex]
			if pd.ID != created.ID || normal.ID != ordinary.ID {
				t.Fatalf("sort %s: %v", query, pageNames(page))
			}
			if pd.Replicas != 3 || pd.ReadyReplicas == nil || *pd.ReadyReplicas != ready {
				t.Fatalf("PD list counts = %s", rec.Body.String())
			}
			if normal.Replicas != 2 || normal.ReadyReplicas == nil || *normal.ReadyReplicas != 0 {
				t.Fatalf("ordinary counts = %+v", normal)
			}
			if string(wire.Items[pdIndex]["replicas"]) != "3" || wire.Items[pdIndex]["ready_replicas"] == nil {
				t.Fatalf("wire counts = %s", rec.Body.String())
			}
			if _, ok := wire.Items[pdIndex]["pd"]; ok {
				t.Fatal("list row carries full PD configuration")
			}
		}
	}
	check(0)
	nodeB, err := st.NodeByUUID("pd-b")
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range []store.Instance{
		{UUID: "prefill-ready", NodeID: nodeA.ID, GPUIndexes: "[0]", State: proto.InstRunning, Role: "prefill"},
		{UUID: "prefill-failed", NodeID: nodeA.ID, GPUIndexes: "[1]", State: proto.InstFailed, Role: "prefill"},
		{UUID: "decode-ready", NodeID: nodeB.ID, GPUIndexes: "[0,1]", State: proto.InstRunning, Role: "decode"},
	} {
		inst.DeploymentID = created.ID
		if _, err := st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	check(2)
	// The list derives its total from the same role counts as the detail.
	detail := h.deploymentDTO(mustDeployment(t, st, created.ID), true)
	if detail.PD == nil {
		t.Fatal("PD detail missing")
	}
	total, ready := 0, 0
	for _, role := range detail.PD.Roles {
		total += role.Desired
		ready += role.Ready
	}
	if total != 3 || ready != 2 {
		t.Fatalf("detail roles = %+v", detail.PD.Roles)
	}
	if err := st.SetInstanceState("prefill-failed", proto.InstRunning, "recovered"); err != nil {
		t.Fatal(err)
	}
	check(3)
	// Reading the list must not rewrite the ordinary-only stored replica field.
	if dep := mustDeployment(t, st, created.ID); dep.Replicas != 1 {
		t.Fatalf("stored PD replicas = %d", dep.Replicas)
	}
}

func TestDeploymentListItemPDTotalsExcludeFollowerRanks(t *testing.T) {
	row := deploymentListItem(dto.Deployment{Replicas: 1, PD: &dto.DeploymentPD{Roles: []dto.DeploymentPDRole{
		{Role: "decode", Desired: 1, Ready: 1, Units: []dto.DeploymentPDUnit{{Followers: []dto.DeploymentPDRank{{NodeUUID: "follower"}}}}},
		{Role: "prefill", Desired: 1, Ready: 1},
	}}}, nil)
	if row.Replicas != 2 || row.ReadyReplicas == nil || *row.ReadyReplicas != 2 {
		t.Fatalf("list row = %+v", row)
	}
}
