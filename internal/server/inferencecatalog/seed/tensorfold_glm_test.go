package seed

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

const (
	tensorfoldGLMModel = "mia-ailab/glm-5.3-flash"
	tensorfoldGB10Spec = "glm-5.3-flash-tensorfold-exl3-gb10-tp2"
	tensorfoldTP4Spec  = "glm-5.3-flash-tensorfold-exl3-rtx-pro-6000-tp4"
)

// The four-card profile is one recipe for both the 96 GB RTX PRO 6000 and the
// 84 GB RTX 6000D (whose name carries no "PRO"); the Ada RTX 6000 shares the
// name stem and is kept out by compute capability alone.
func TestTensorFoldTP4ProfileMatchesBothBlackwell6000Cards(t *testing.T) {
	_, spec := catalogSpec(t, tensorfoldGLMModel, tensorfoldTP4Spec)
	predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
	for _, card := range []struct {
		raw, cc string
		want    bool
	}{
		{"NVIDIA RTX PRO 6000 Blackwell Server Edition", "12.0", true},
		{"NVIDIA RTX PRO 6000 Blackwell Max-Q Workstation Edition", "12.0", true},
		{"NVIDIA RTX 6000D", "12.0", true},
		{"NVIDIA RTX 6000 Ada Generation", "8.9", false},
		{"NVIDIA GeForce RTX 5090", "12.0", false},
	} {
		gpu := chipmatch.ReportedGPU(accelerator.NormalizeGPUName(card.raw), "nvidia", "", card.cc)
		if got := chipmatch.Evaluate(gpu, predicate).Matched; got != card.want {
			t.Errorf("%s (cc %s) matched = %v, want %v", card.raw, card.cc, got, card.want)
		}
	}
}

// TensorFold's GLM layout is exactly four ranks, one per card, and the
// weights alone take ~44 GB of each: the resolver admits four cards of 80 GiB
// or more (the 6000D reports 83.6 GiB) and nothing else.
func TestTensorFoldTP4ProfileAdmitsExactlyFourLargeCards(t *testing.T) {
	_, spec := catalogSpec(t, tensorfoldGLMModel, tensorfoldTP4Spec)
	resolve := func(count int, memoryGiB float64) placement.Result {
		t.Helper()
		names := make([]string, count)
		memories := make([]float64, count)
		for index := range names {
			names[index], memories[index] = "NVIDIA RTX 6000D", memoryGiB
		}
		result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
			Count: count, Vendor: "nvidia", Names: names, MemoriesGiB: memories, MinMemoryGiB: memoryGiB,
		})
		if err != nil {
			t.Fatalf("count=%d memory=%v: %v", count, memoryGiB, err)
		}
		return result
	}
	for _, memory := range []float64{83.6, 95.6} {
		result := resolve(4, memory)
		if result.Reason != "" {
			t.Fatalf("4 cards of %v GiB rejected: %s", memory, result.Reason)
		}
		if result.GPUMemoryMiBPerDevice != 72*1024 {
			t.Errorf("4 cards of %v GiB admit %d MiB per card, want %d", memory, result.GPUMemoryMiBPerDevice, 72*1024)
		}
	}
	for _, count := range []int{1, 2, 8} {
		if result := resolve(count, 95.6); result.Reason == "" {
			t.Errorf("%d cards accepted; the recipe runs exactly four ranks", count)
		}
	}
	if result := resolve(4, 47.5); result.Reason == "" {
		t.Error("4 cards of 48 GB accepted")
	}
}

// The GB10 profile runs one rank per Spark and places the rendezvous itself:
// on one node there is no master address and the launch is refused instead of
// started; on two nodes rank 0 serves the API and rank 1 is headless.
func TestTensorFoldGB10ProfileLaunchesOnlyAcrossTwoNodes(t *testing.T) {
	_, spec := catalogSpec(t, tensorfoldGLMModel, tensorfoldGB10Spec)
	contract, err := inferenceapi.EffectiveContract(spec.API, inferenceapi.DefaultsInput{Engine: spec.Engine})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	launch := func(params deploy.Params) ([]string, error) {
		t.Helper()
		params.ModelPath, params.TemplateArgs, params.TemplateEnv = "/model", spec.Args, spec.Env
		resolved, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
			Engine: engine, Params: params, Entrypoint: spec.Entrypoint, APIContract: contract,
		})
		return resolved.Argv, err
	}

	if _, err := launch(deploy.Params{APIPort: 30000, TensorParallel: 1}); err == nil || !strings.Contains(err.Error(), "MASTER_ADDR") {
		t.Fatalf("single-node launch error = %v, want the unresolved MASTER_ADDR", err)
	}
	// A two-node deployment that states no TP gets one pipeline stage per
	// node (parallelism.Spec.Defaulted: TP 1 x PP 2); TensorFold still needs
	// --tp 2, so the template takes the node count rather than TP_SIZE.
	leader, err := launch(deploy.Params{APIPort: 30000, TensorParallel: 1, PipelineParallel: 2, NNodes: 2, MasterAddr: "10.43.4.1", MasterPort: 29500})
	if err != nil {
		t.Fatal(err)
	}
	follower, err := launch(deploy.Params{TensorParallel: 1, PipelineParallel: 2, NNodes: 2, NodeRank: 1, Headless: true, MasterAddr: "10.43.4.1", MasterPort: 29500})
	if err != nil {
		t.Fatal(err)
	}
	if stated, err := launch(deploy.Params{APIPort: 30000, TensorParallel: 2, NNodes: 2, MasterAddr: "10.43.4.1", MasterPort: 29500}); err != nil || !containsPair(stated, "--tp", "2") {
		t.Fatalf("a stated TP2 launch argv %v (err %v) lacks --tp 2", stated, err)
	}
	for _, want := range [][]string{
		{"--port", "30000"}, {"--tp", "2"}, {"--rank", "0"}, {"--headless", "0"}, {"--master", "10.43.4.1"}, {"--master-port", "29500"},
	} {
		if !containsPair(leader, want[0], want[1]) {
			t.Errorf("rank 0 argv %v lacks %s %s", leader, want[0], want[1])
		}
	}
	for _, want := range [][]string{{"--tp", "2"}, {"--rank", "1"}, {"--headless", "1"}, {"--port", "0"}} {
		if !containsPair(follower, want[0], want[1]) {
			t.Errorf("rank 1 argv %v lacks %s %s", follower, want[0], want[1])
		}
	}
}

// Both profiles mount the same DFlash2 draft at /draft, which their argv names
// literally; the draft is a catalog model of its own, so a deployment can
// resolve and download it like the primary weights.
func TestTensorFoldProfilesMountTheCatalogDFlash2Draft(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	models := map[string]inferencecatalog.InferenceModel{}
	for _, model := range catalog.Models {
		models[model.ModelID] = model
	}
	for _, name := range []string{tensorfoldGB10Spec, tensorfoldTP4Spec} {
		_, spec := catalogSpec(t, tensorfoldGLMModel, name)
		if !containsPair(spec.Args, "--drafter", "/draft") {
			t.Errorf("%s argv %v does not name the draft mount", name, spec.Args)
		}
		var draft *inferencecatalog.Component
		for index := range spec.Components {
			if spec.Components[index].MountPath == "/draft" {
				draft = &spec.Components[index]
			}
		}
		if draft == nil {
			t.Errorf("%s mounts nothing at /draft", name)
			continue
		}
		if _, ok := models[draft.ModelID]; !ok {
			t.Errorf("%s draft component names %q, which the catalog does not carry", name, draft.ModelID)
		}
		if draft.WeightBytes != 2342460697 {
			t.Errorf("%s draft weight_bytes = %d, want the pinned revision's 2342460697", name, draft.WeightBytes)
		}
		for _, source := range draft.Weights.Sources {
			if !strings.EqualFold(source.RepoID, "incoai/GLM-5.3-Flash-DFlash2") {
				t.Errorf("%s draft source %s:%s is not the DFlash2 repository", name, source.Provider, source.RepoID)
			}
		}
	}
}

func containsPair(argv []string, flag, value string) bool {
	for index := range argv {
		if argv[index] == flag && index+1 < len(argv) && argv[index+1] == value {
			return true
		}
	}
	return slices.Contains(argv, flag+"="+value)
}

// Every family the TensorFold specs map is one TensorFold 0.6.0 actually
// exposes - checked against a real /metrics response, so a renamed metric
// fails here rather than as an empty panel - and the declaration resolves to
// an observation that maps each of them.
func TestTensorFoldMetricsMapRealExpositionFamilies(t *testing.T) {
	raw, err := os.ReadFile("../../../inferencemetrics/testdata/tensorfold-gb10.metrics")
	if err != nil {
		t.Fatal(err)
	}
	exposed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if end := strings.IndexAny(line, "{ "); end >= 0 {
			name = line[:end]
		}
		exposed[name] = true
	}
	for _, name := range []string{tensorfoldGB10Spec, tensorfoldTP4Spec} {
		_, spec := catalogSpec(t, tensorfoldGLMModel, name)
		if spec.API == nil || spec.API.Metrics == nil || len(spec.API.Metrics.Map) == 0 {
			t.Fatalf("%s declares no metrics", name)
		}
		for source, target := range spec.API.Metrics.Map {
			definition, ok := inferencemetrics.Lookup(target.To)
			if !ok {
				t.Fatalf("%s maps %s to unknown %s", name, source, target.To)
			}
			family := source
			if definition.Kind == inferencemetrics.Histogram {
				family += "_bucket"
			}
			if !exposed[family] {
				t.Errorf("%s maps %s, which TensorFold does not expose", name, source)
			}
		}
		observation := deploy.ResolveObservation(spec.Engine, spec.API.Metrics)
		if len(observation.Instance.MetricRules) != len(spec.API.Metrics.Map) || len(observation.Instance.MetricDetect) != 0 {
			t.Fatalf("%s resolves to %+v", name, observation.Instance)
		}
	}
}
