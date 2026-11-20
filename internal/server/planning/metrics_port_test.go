package planning

import (
	"context"
	"slices"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template that declares api.metrics.port: separate places the port with
// ${GPUWORKS_METRICS_PORT}. The controller's launch states the port it
// reserved; the preview, which reserves none, shows a placeholder; a headless
// rank has none and says zero, like its API port.
func TestPlanLaunchStatesTheMetricsPortOfATemplateThatDeclaresOne(t *testing.T) {
	st, err := store.Open("file:planning_metrics_port?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/metrics-port", Name: "metrics-port", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("metrics-port-node", store.HashToken("token"), "metrics-port-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	selection := func(api string) deploy.CatalogSelection {
		selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296,` + api +
			`"extra_args":["--dtype","bfloat16","--metrics-listen","${GPUWORKS_METRICS_PORT}"]}`))
		if err != nil {
			t.Fatal(err)
		}
		return selection
	}
	declared := selection(`"api":{"metrics":{"port":"separate"}},`)
	base := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: declared,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		Quick: deploy.LaunchConfig{MaxModelLen: 4096}, ModelMount: deploy.DefaultModelMount, ServedName: "fixture",
		Source: LaunchSourceFromRequest("registry.example/app:v1", deploy.ContainerOverrides{}),
	}
	metricsFlag := func(t *testing.T, req LaunchRequest) string {
		t.Helper()
		plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
		if err != nil {
			t.Fatal(err)
		}
		all := append(slices.Clone(plan.Resolved.Entrypoint), plan.Resolved.Argv...)
		index := slices.Index(all, "--metrics-listen")
		if index < 0 || index+1 >= len(all) {
			t.Fatalf("no --metrics-listen in %v", all)
		}
		return all[index+1]
	}

	launch := base
	launch.APIPort, launch.MetricsPort = 31000, 31001
	if got := metricsFlag(t, launch); got != "31001" {
		t.Fatalf("launch metrics port = %s, want the reserved 31001", got)
	}
	preview := base
	preview.Preview = true
	if got := metricsFlag(t, preview); got != strconv.Itoa(PreviewMetricsPort) {
		t.Fatalf("preview metrics port = %s, want %d", got, PreviewMetricsPort)
	}

	if !launch.NeedsMetricsPort() || launch.Topology.Headless {
		t.Fatal("a serving rank of a template that declares the port needs one")
	}
	headless := base
	headless.Topology = DistTopology{NNodes: 2, NodeRank: 1, Role: "follower", Headless: true}
	if headless.NeedsMetricsPort() {
		t.Fatal("a headless rank was given a metrics port")
	}
	undeclared := base
	undeclared.Selection = selection("")
	if undeclared.NeedsMetricsPort() {
		t.Fatal("a template that declared no port was given one")
	}
}
