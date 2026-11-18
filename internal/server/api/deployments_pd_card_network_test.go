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
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluatePDWarnsAboutSelectedCardConnectivity(t *testing.T) {
	h, modelID, st := pdAPIFixture(t)
	network, err := st.CreateNetwork(&store.Network{Name: "fabric", Type: netplan.TypeAcceleratorRDMA, Roles: `["compute"]`})
	if err != nil {
		t.Fatal(err)
	}
	for i, uuid := range []string{"pd-a", "pd-b"} {
		status := nodeStatusTwoGPUs(uuid, i)
		status.Sys = proto.SysInfo{Hostname: uuid, Arch: "arm64", GPUVendor: "ascend", Framework: "cann", FrameworkVersion: "9.1"}
		status.GPUs = []proto.GPUInfo{{Index: 0, Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", MemoryTotalBytes: 32 << 30, Healthy: true}}
		status.Network.AcceleratorPorts = []proto.AcceleratorPort{{Name: "npu0", Index: 0, IP: fmt.Sprintf("172.20.9.%d", i+1), Netmask: "255.255.0.0", State: "UP"}}
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
	body := fmt.Sprintf(`{"name":"card-check","model_id":%d,"engine":"vllm","image":"gpuworks/gpuworks-runner:cann9.1-910b-vllm0.26.0rc1-r2","quick_params":{"max_model_len":4096},"placement":{"pd":{"mode":"vllm-ascend-mooncake","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-a","gpus":[0]}]},"decode":{"tp":1,"units":[{"node_uuid":"pd-b","gpus":[0]}]}}}}}`, modelID)
	evaluate := func(want string) {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		var response dto.EvaluateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rec.Code != http.StatusOK || !response.OK {
			t.Fatalf("evaluation failed before card warning: %s", rec.Body.String())
		}
		for _, warning := range response.Warnings {
			if warning.Code == want {
				return
			}
		}
		t.Fatalf("missing %s: %s", want, rec.Body.String())
	}
	evaluate(messages.WarningPDCardNetworkUntested)
	run, err := st.StartNetworkTestRun(&store.NetworkTestRun{NetworkID: network.ID, Kind: "card_matrix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishNetworkTestRun(run.ID, `[{"source_uuid":"pd-a","target_uuid":"pd-b","reachable":true,"rails":[{"rail":"npu0","target_rail":"npu0","source_ip":"172.20.9.1","target_ip":"172.20.9.2","code":"unreachable"}]}]`, 1, 0); err != nil {
		t.Fatal(err)
	}
	evaluate(messages.WarningPDCardNetworkUnreachable)
}
