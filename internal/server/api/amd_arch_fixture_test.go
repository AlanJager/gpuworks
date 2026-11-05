package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Keep the UI's hardware refusal example on the real evaluate response path.
// The fixture contains only the selected node's structured refusal, not a
// second implementation of GPU predicate matching.
func TestAMDHardwareRefusalMatchesUIFixture(t *testing.T) {
	// The shared fixture changes the process-wide default image. Restore it
	// so this AMD case cannot change later vendors' image warning contracts.
	profile, err := deploy.ProfileForEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deploy.SetDefaultImage("vllm", profile.DefaultImage) })
	h, templateID := evaluateFixtureWorld(t)
	catalog, err := h.d.Store.InferenceModelByModelID("acme/fixture")
	if err != nil {
		t.Fatal(err)
	}
	catalog.Specs = append(catalog.Specs, store.InferenceSpecRow{
		Name: "amd-gfx", Scenario: "balanced", Engine: "vllm", Image: "example.invalid/rocm:fixture", Quantization: "fp8", WeightBytes: 16 << 30,
		WeightSources: `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`, GPUs: `{"vendor":"amd","arch_id":"gfx1151"}`,
	})
	if err := h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{*catalog}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.RegisterNode("fixture-amd", store.HashToken("token"), "fixture-amd"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus("fixture-amd", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "fixture-amd", Arch: "amd64", GPUVendor: "amd", Framework: "rocm", FrameworkVersion: "6.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.RegisterNode("fixture-amd-fit", store.HashToken("token"), "fixture-amd-fit"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus("fixture-amd-fit", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "fixture-amd-fit", Arch: "amd64", GPUVendor: "amd", Framework: "rocm", FrameworkVersion: "6.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Radeon 8060S", Vendor: "amd", ArchID: "gfx1151", MemoryTotalBytes: 64 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"amd-gfx","node_uuid":"fixture-amd-fit","target_vendor":"amd"}`, templateID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		EligibleNodes []struct {
			UUID    string            `json:"uuid"`
			Reasons []json.RawMessage `json:"reasons"`
		} `json:"eligible_nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, node := range response.EligibleNodes {
		if node.UUID != "fixture-amd" {
			continue
		}
		for _, reason := range node.Reasons {
			var wire struct {
				Code   string         `json:"code"`
				Params map[string]any `json:"params"`
			}
			if err := json.Unmarshal(reason, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Code != "chip_mismatch" {
				continue
			}
			if wire.Params["required_arch_id"] != "gfx1151" {
				t.Fatalf("required facts=%v", wire.Params)
			}
			facts, ok := wire.Params["actual_gpus"].([]any)
			if !ok || len(facts) != 1 || facts[0].(map[string]any)["arch_id"] != "gfx942" {
				t.Fatalf("actual facts=%v", wire.Params)
			}
			update := os.Getenv(evaluateFixturesEnv) != ""
			checkUIFixture(t, evaluateFixturesDir, "amd_chip_mismatch_reason", reason, update)
			if update {
				t.Fatal("regenerated AMD hardware refusal fixture; rerun without GPUWORKS_UPDATE_UI_FIXTURES")
			}
			return
		}
	}
	t.Fatalf("missing AMD chip refusal: %s", rec.Body.String())
}
