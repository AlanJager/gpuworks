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
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func pdCardAdmissionFixture(t *testing.T) (*apiHandlers, uint, *store.Store, uint) {
	t.Helper()
	h, modelID, st := pdAPIFixture(t)
	network, err := st.CreateNetwork(&store.Network{Name: "fabric", Type: netplan.TypeAcceleratorRDMA, Roles: `["compute"]`})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"pd-a", "pd-b"} {
		status := nodeStatusTwoGPUs(uuid, i)
		status.Sys = proto.SysInfo{Hostname: uuid, Arch: "arm64", GPUVendor: "ascend", Framework: "cann", FrameworkVersion: "9.1"}
		status.Network.AcceleratorPorts = nil
		for card := range status.GPUs {
			status.GPUs[card] = proto.GPUInfo{Index: card, Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}
			status.Network.AcceleratorPorts = append(status.Network.AcceleratorPorts, proto.AcceleratorPort{
				Name: fmt.Sprintf("npu%d", card), Index: card, IP: fmt.Sprintf("172.20.9.%d", i*2+card+1), Netmask: "255.255.0.0", State: "UP",
			})
		}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
		node, err := st.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateNetworkAttachment(&store.NetworkAttachment{NetworkID: network.ID, NodeID: node.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return h, modelID, st, network.ID
}

func pdCardAdmissionBody(modelID uint, prefillTP int, prefillCards string) string {
	return fmt.Sprintf(`{"name":"card-check","model_id":%d,"engine":"vllm","image":"gpuworks/gpuworks-runner:cann9.1-910b-vllm0.26.0rc1-r2","quick_params":{"max_model_len":4096},"placement":{"pd":{"mode":"vllm-ascend-mooncake","roles":{"prefill":{"tp":%d,"units":[{"node_uuid":"pd-a","gpus":%s}]},"decode":{"tp":1,"units":[{"node_uuid":"pd-b","gpus":[1]}]}}}}}`, modelID, prefillTP, prefillCards)
}

func TestPDAdmissionUsesMeasuredCardConnectivity(t *testing.T) {
	h, modelID, st, networkID := pdCardAdmissionFixture(t)
	body := pdCardAdmissionBody(modelID, 1, "[0]")
	evaluate := func(wantOK bool) dto.EvaluateResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		var response dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || response.OK != wantOK {
			t.Fatalf("evaluate = %d %s, want ok=%t", rec.Code, rec.Body.String(), wantOK)
		}
		return response
	}
	create := func(wantStatus int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
		if rec.Code != wantStatus {
			t.Fatalf("create = %d %s, want %d", rec.Code, rec.Body.String(), wantStatus)
		}
		return rec
	}
	startProbe := func(kind string) uint {
		t.Helper()
		run, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: networkID, Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		return run.ID
	}
	finishProbe := func(id uint, rail string, passed int) {
		t.Helper()
		raw := fmt.Sprintf(`[{"source_uuid":"pd-a","target_uuid":"pd-b","rails":[%s]}]`, rail)
		if err := st.FinishNetworkTestRun(id, raw, 1, passed); err != nil {
			t.Fatal(err)
		}
	}

	untested := evaluate(true)
	if !slices.ContainsFunc(untested.Warnings, func(w messages.Warning) bool {
		return w.Code == messages.WarningPDCardNetworkUntested
	}) {
		t.Fatalf("untested placement warnings = %+v", untested.Warnings)
	}
	const failedCrossCard = `{"rail":"npu0","target_rail":"npu1","source_ip":"172.20.9.1","target_ip":"172.20.9.4","code":"unreachable"}`
	finishProbe(startProbe("card_matrix"), failedCrossCard, 0)
	finishProbe(startProbe("card_ping"), `{"rail":"npu0","target_rail":"npu0","source_ip":"172.20.9.1","target_ip":"172.20.9.3","reachable":true}`, 1)
	evaluate(false)
	finishProbe(startProbe("card_matrix"), `{"rail":"npu0","target_rail":"npu1","source_ip":"172.20.9.1","target_ip":"172.20.9.4","code":"tool_failed"}`, 0)
	blocked := evaluate(false)
	if len(blocked.Errors) != 1 || blocked.Errors[0].Field != "placement.pd" ||
		blocked.Errors[0].Code != messages.ErrorPDCardNetworkBlocked ||
		blocked.Errors[0].Params["pairs"] != "pd-a/npu0 (172.20.9.1) ↔ pd-b/npu1 (172.20.9.4)" {
		t.Fatalf("unreachable placement errors = %+v", blocked.Errors)
	}
	retest := startProbe("card_matrix")
	refusal := create(http.StatusBadRequest)
	if !strings.Contains(refusal.Body.String(), `"code":"`+messages.ErrorPDCardNetworkBlocked+`"`) {
		t.Fatalf("create refusal = %s", refusal.Body.String())
	}
	deps, err := st.ListDeployments()
	if err != nil || len(deps) != 0 {
		t.Fatalf("refusal left deployments: %v, error: %v", deps, err)
	}

	finishProbe(retest, `{"rail":"npu0","target_rail":"npu1","source_ip":"172.20.9.1","target_ip":"172.20.9.4","reachable":true}`, 1)
	reachable := evaluate(true)
	for _, w := range reachable.Warnings {
		if w.Code == messages.WarningPDCardNetworkUnreachable || w.Code == messages.WarningPDCardNetworkUntested {
			t.Fatalf("reachable placement has stale warning: %+v", w)
		}
	}
	create(http.StatusCreated)
}

func TestPDAdmissionWarnsForMultipleCards(t *testing.T) {
	h, modelID, st, networkID := pdCardAdmissionFixture(t)
	run, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: networkID, Kind: "card_matrix"})
	if err != nil {
		t.Fatal(err)
	}
	result := `[{"source_uuid":"pd-a","target_uuid":"pd-b","rails":[
		{"rail":"npu0","target_rail":"npu1","source_ip":"172.20.9.1","target_ip":"172.20.9.4","code":"unreachable"},
		{"rail":"npu1","target_rail":"npu1","source_ip":"172.20.9.2","target_ip":"172.20.9.4","code":"unreachable"}
	]}]`
	if err := st.FinishNetworkTestRun(run.ID, result, 1, 0); err != nil {
		t.Fatal(err)
	}
	body := pdCardAdmissionBody(modelID, 2, "[0,1]")
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	var response dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || !response.OK || !slices.ContainsFunc(response.Warnings, func(w messages.Warning) bool {
		return w.Code == messages.WarningPDCardNetworkUnreachable
	}) {
		t.Fatalf("evaluate must warn without blocking multi-card PD: %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create for multi-card PD = %d %s", rec.Code, rec.Body.String())
	}
}
