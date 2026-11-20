package api

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Keep the real adapter's assembly and hooks, with one architecture-dependent
// launch option. This exposes callers that resolve without actual card facts.
type architectureFeatureEngine struct {
	deploy.Engine
	deploy.EngineHooks
}

func (e architectureFeatureEngine) Build(p deploy.Params) deploy.ResolvedLaunch {
	launch := e.Engine.Build(p)
	if p.ArchID == "gfx1151" {
		launch.Argv = append(launch.Argv, "--data-parallel-size", "2")
	}
	return launch
}

func TestEvaluateEligibilityUsesActualArchitectureForTemplateLaunch(t *testing.T) {
	original, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	deploy.RegisterEngine(architectureFeatureEngine{Engine: original, EngineHooks: original.(deploy.EngineHooks)})
	defer deploy.RegisterEngine(original)
	h, _ := eligibleNodeFixture(t)
	for _, arch := range []string{"gfx942", "gfx1151"} {
		uuid := "eligible-" + arch
		if _, err := h.d.Store.RegisterNode(uuid, store.HashToken(uuid), uuid); err != nil {
			t.Fatal(err)
		}
		if err := h.d.Store.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: proto.SysInfo{Hostname: uuid, OS: "linux", Arch: "amd64", GPUVendor: "amd", Framework: "rocm", RuntimeDriver: "docker"}, GPUs: []proto.GPUInfo{{Index: 0, Vendor: "amd", Name: "AMD GPU " + arch, ArchID: arch, Healthy: true, MemoryTotalBytes: 96 << 30}}}); err != nil {
			t.Fatal(err)
		}
	}
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/architecture-feature"
	fixture.Unsupported = ""
	fixture.Specs[0].GPUs = `{"vendor":"amd"}`
	fixture.Specs[0].Quantization = "bf16"
	fixture.Specs[0].Entrypoint = `["vllm","serve","${MODEL_PATH}"]`
	if err := h.d.Store.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := h.d.Store.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	nodes := eligibleNodesOf(t, h, fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","target_vendor":"amd","placement_mode":"auto","quick_params":{"max_model_len":512}}`, template.ID))
	if !nodes["eligible-gfx942"].Eligible {
		t.Fatalf("ordinary target rejected: %+v", nodes["eligible-gfx942"])
	}
	// The gfx1151 launch resolves to data parallelism, which AMD runs: the
	// target accepts it. Its one card cannot hold the two workers, and that -
	// the launch as its real architecture resolves it - is all it is turned
	// down for.
	gfx1151 := nodes["eligible-gfx1151"]
	if gfx1151.Eligible || len(gfx1151.Reasons) != 1 || gfx1151.Reasons[0]["code"] != "placement_rejected" || !strings.Contains(fmt.Sprint(gfx1151.Reasons[0]["params"]), "data parallel 2") {
		t.Fatalf("architecture-dependent resolved DP launch = %+v, want it turned down only for its card count", gfx1151)
	}
	// The reason is the error a create naming the node answers, without the
	// planner's own prefix, and the node is judged the same when it is the
	// node the draft has: the list does not flip with the selection.
	if detail := fmt.Sprint(gfx1151.Reasons[0]["params"]); strings.Contains(detail, "deployment 0") || !strings.Contains(detail, "resolve launch: vllm topology: ") {
		t.Fatalf("reason detail = %s", detail)
	}
	for _, selected := range []string{"eligible-gfx942", "eligible-gfx1151"} {
		nodes = eligibleNodesOf(t, h, fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","target_vendor":"amd","placement_mode":"manual","node_uuid":%q,"quick_params":{"max_model_len":512}}`, template.ID, selected))
		if !nodes["eligible-gfx942"].Eligible || nodes["eligible-gfx1151"].Eligible || !reflect.DeepEqual(nodes["eligible-gfx1151"].Reasons, gfx1151.Reasons) {
			t.Fatalf("with %s selected: gfx942=%+v gfx1151=%+v, want the list of the automatic draft", selected, nodes["eligible-gfx942"], nodes["eligible-gfx1151"])
		}
	}
}
