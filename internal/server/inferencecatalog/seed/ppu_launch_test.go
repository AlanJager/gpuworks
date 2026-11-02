package seed

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

// ppuDeviceName is a device that satisfies the GPU predicate these profiles
// carry, so a vector is defined by its card count and vendor rather than by a
// name the predicate would have rejected first.
const ppuDeviceName = "PPU-M890"

// ppuPinnedCardCount matches a resolver that only accepts one exact card count.
var ppuPinnedCardCount = regexp.MustCompile(`gpu\.count != (\d+)`)

type ppuTopologyExpectation struct {
	model   string
	spec    string
	count   int
	reject  []int
	tp      int
	pp      int
	ppInArg bool
}

// ppuFixedTopologyProfiles are the PPU profiles whose passing run pinned a
// multi-GPU topology. Each must keep its exact card count: a resolver that
// grows a wildcard would silently publish a TP4-proved checkpoint as "any
// count", which is the defect these profiles exist to prevent.
var ppuFixedTopologyProfiles = []ppuTopologyExpectation{
	{model: "qwen/qwen3-235b-a22b-instruct-2507", spec: "bf16-vllm-ppu-tp8-ac9c66cc", count: 8, reject: []int{1, 2, 4}, tp: 8, pp: 1},
	{model: "qwen/qwen3.5-397b-a17b", spec: "int4-vllm-ppu-tp4-df333de3", count: 4, reject: []int{1, 2, 8}, tp: 4, pp: 1},
	{model: "poolside/laguna-s-2.1", spec: "int4-vllm-ppu-tp2-a6977d07", count: 2, reject: []int{1, 4, 8}, tp: 2, pp: 1},
	{model: "deepseek-ai/deepseek-v4-flash", spec: "fp8-vllm-ppu-pp4-60d8d707", count: 4, reject: []int{1, 2, 8}, tp: 1, pp: 4, ppInArg: true},
}

func catalogSpec(t *testing.T, modelID, specName string) (inferencecatalog.InferenceModel, inferencecatalog.InferenceSpec) {
	t.Helper()
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != modelID {
			continue
		}
		for _, spec := range model.Specs {
			if spec.Name == specName {
				return model, spec
			}
		}
		t.Fatalf("%s is missing spec %s", modelID, specName)
	}
	t.Fatalf("catalog is missing model %s", modelID)
	return inferencecatalog.InferenceModel{}, inferencecatalog.InferenceSpec{}
}

// ppuResolve runs a profile resolver against a concrete PPU vector. The
// per-device memory is deliberately generous so an accepted count fails for a
// topology reason rather than a capacity one.
func ppuResolve(t *testing.T, spec inferencecatalog.InferenceSpec, count int) placement.Result {
	t.Helper()
	names := make([]string, count)
	memories := make([]float64, count)
	for index := range names {
		names[index], memories[index] = ppuDeviceName, 96
	}
	result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
		Count: count, Vendor: spec.GPUs.Vendor, Names: names, MemoriesGiB: memories, MinMemoryGiB: 64,
	})
	if err != nil {
		t.Fatalf("%s/%s count=%d: %v", spec.Name, spec.GPUs.Vendor, count, err)
	}
	return result
}

func TestPPUFixedTopologyProfilesAcceptOnlyTheirProvedCardCount(t *testing.T) {
	for _, expected := range ppuFixedTopologyProfiles {
		t.Run(expected.model, func(t *testing.T) {
			_, spec := catalogSpec(t, expected.model, expected.spec)
			accepted := ppuResolve(t, spec, expected.count)
			if accepted.Reason != "" {
				t.Fatalf("%s rejects its proved %d-GPU topology: %s", spec.Name, expected.count, accepted.Reason)
			}
			// The resolver's own variables must agree with the launch flags. A
			// profile that pinned TP_SIZE but left PP_SIZE implicit would expand
			// --pipeline-parallel-size to the default 1 and under-consume the
			// assigned world.
			if got := accepted.Variables["TP_SIZE"]; got != int64(expected.tp) {
				t.Errorf("%s TP_SIZE=%v, want %d", spec.Name, got, expected.tp)
			}
			if got := accepted.Variables["PP_SIZE"]; got != int64(expected.pp) {
				t.Errorf("%s PP_SIZE=%v, want %d", spec.Name, got, expected.pp)
			}
			for _, count := range expected.reject {
				if rejected := ppuResolve(t, spec, count); rejected.Reason == "" {
					t.Errorf("%s accepts %d GPUs; only the proved %d-GPU topology may be published", spec.Name, count, expected.count)
				}
			}
		})
	}
}

func TestPPUFixedTopologyProfilesLaunchTheProvedTopology(t *testing.T) {
	for _, expected := range ppuFixedTopologyProfiles {
		t.Run(expected.model, func(t *testing.T) {
			_, spec := catalogSpec(t, expected.model, expected.spec)
			accepted := ppuResolve(t, spec, expected.count)
			engine, err := deploy.SelectEngine(spec.Engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine,
				Params: deploy.Params{
					ModelPath: "/model", APIPort: 30000,
					// The scheduler assigns exactly the GPUs the resolver demanded,
					// so the local tensor parallel degree is their count.
					TensorParallel: expected.count, TemplateArgs: spec.Args, TemplateEnv: spec.Env,
				},
				PlacementVariables: accepted.Variables,
				APIContract:        spec.API,
			})
			if err != nil {
				t.Fatal(err)
			}
			command := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
			// A topology flag may appear exactly once. A second copy would mean the
			// engine injected its own value alongside the profile's, and the engine
			// keeps whichever it parses last - so counting is the assertion, not just
			// reading the first match.
			if got := commandFlagValue(command, "--tensor-parallel-size"); got != strconv.Itoa(expected.tp) {
				t.Errorf("%s launched --tensor-parallel-size %q, want %d: %v", spec.Name, got, expected.tp, command)
			}
			if count := commandFlagCount(command, "--tensor-parallel-size"); count != 1 {
				t.Errorf("%s has %d --tensor-parallel-size flags, want 1: %v", spec.Name, count, command)
			}
			got := commandFlagValue(command, "--pipeline-parallel-size")
			count := commandFlagCount(command, "--pipeline-parallel-size")
			if expected.ppInArg {
				if got != strconv.Itoa(expected.pp) || count != 1 {
					t.Errorf("%s launched --pipeline-parallel-size %q (%d flags), want %d once: %v", spec.Name, got, count, expected.pp, command)
				}
				return
			}
			if count != 0 {
				t.Errorf("%s launched an unexpected --pipeline-parallel-size %q: %v", spec.Name, got, command)
			}
		})
	}
}

// TestPPUPipelineProfileRefusesToLaunchWithoutPipelineParallelism is the negative
// half of the pipeline contract above. The catalog profile is the only layer
// that supplies pipeline parallelism, so a spec edit that drops the
// placeholder, or pinning it to a single stage, is the failure mode that
// would publish a PP4-proved checkpoint as a single-stage launch: the launch
// has to stop rather than quietly consume fewer GPUs than placement assigned.
func TestPPUPipelineProfileRefusesToLaunchWithoutPipelineParallelism(t *testing.T) {
	_, spec := catalogSpec(t, "deepseek-ai/deepseek-v4-flash", "fp8-vllm-ppu-pp4-60d8d707")
	if !containsArgumentPair(spec.Args, "--tensor-parallel-size", "${TP_SIZE}") ||
		!containsArgumentPair(spec.Args, "--pipeline-parallel-size", "${PP_SIZE}") {
		t.Fatalf("pipeline profile must reference both topology placeholders: %v", spec.Args)
	}
	accepted := ppuResolve(t, spec, 4)
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
	}{
		{name: "pipeline parallel placeholder removed", args: dropArgumentPair(spec.Args, "--pipeline-parallel-size", "${PP_SIZE}")},
		{name: "pipeline parallel pinned to a single stage", args: replaceArgumentValue(spec.Args, "--pipeline-parallel-size", "1")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
				Engine: engine,
				Params: deploy.Params{
					ModelPath: "/model", APIPort: 30000, TensorParallel: 4,
					TemplateArgs: test.args, TemplateEnv: spec.Env,
				},
				PlacementVariables: accepted.Variables,
				APIContract:        spec.API,
			})
			if err == nil {
				t.Fatal("a pipeline profile that no longer consumes all four assigned GPUs still launched")
			}
			if !strings.Contains(err.Error(), "but placement assigned") {
				t.Fatalf("error=%v, want a placement world-size rejection", err)
			}
		})
	}
}

// TestPPUPipelineProfileRejectsAMultiNodeLaunch pins the other side of the PP4
// contract. The catalog profile owns its own pipeline layout and the launch
// world size counts every node, so spreading the profile over two nodes would
// describe eight GPUs while the profile's own argv still describes four. That
// has to be refused rather than silently sized for the wrong world.
func TestPPUPipelineProfileRejectsAMultiNodeLaunch(t *testing.T) {
	_, spec := catalogSpec(t, "deepseek-ai/deepseek-v4-flash", "fp8-vllm-ppu-pp4-60d8d707")
	accepted := ppuResolve(t, spec, 4)
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	_, err = deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine,
		Params: deploy.Params{
			ModelPath: "/model", APIPort: 30000, AssignedGPUs: 4, TensorParallel: 4,
			NNodes: 2, NodeRank: 0, TemplateArgs: spec.Args, TemplateEnv: spec.Env,
		},
		PlacementVariables: accepted.Variables, APIContract: spec.API,
	})
	if err == nil {
		t.Fatal("a four-GPU pipeline profile launched across two nodes without resizing its world")
	}
	if !strings.Contains(err.Error(), "but placement assigned 8") {
		t.Fatalf("error=%v, want a world-size rejection for the two-node world", err)
	}
}

func commandFlagCount(command []string, flag string) int {
	count := 0
	for _, token := range command {
		if token == flag || strings.HasPrefix(token, flag+"=") {
			count++
		}
	}
	return count
}

func commandFlagValue(command []string, flag string) string {
	for index, token := range command {
		if token == flag && index+1 < len(command) {
			return command[index+1]
		}
		if value, found := strings.CutPrefix(token, flag+"="); found {
			return value
		}
	}
	return ""
}

func dropArgumentPair(args []string, flag, value string) []string {
	out := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		if args[index] == flag && index+1 < len(args) && args[index+1] == value {
			index++
			continue
		}
		out = append(out, args[index])
	}
	return out
}

func replaceArgumentValue(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for index := 0; index+1 < len(out); index++ {
		if out[index] == flag {
			out[index+1] = value
		}
	}
	return out
}

// TestPPUCardCountPinsExpressTopologyNotEvidence keeps the resolver pin from
// drifting back into an evidence claim. The card count a run happened to use
// belongs in the ledger; a resolver may only pin a count when the topology
// itself demands it, and a topology never demands exactly one card - pinning
// one is always "that is the only count we tried", which is what makes a
// profile undeployable on the larger machines the hardware is sold in.
//
// Every remaining pin must therefore be above one and must name its topology,
// so a reader can tell a fixed pipeline layout from a tested configuration.
func TestPPUCardCountPinsExpressTopologyNotEvidence(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	pins := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "ppu" || spec.Placement == nil {
				continue
			}
			match := ppuPinnedCardCount.FindStringSubmatch(spec.Placement.Resolver)
			if match == nil {
				continue
			}
			pins++
			if match[1] == "1" {
				t.Errorf("%s/%s pins a single card; that is an evidence claim, not a topology", model.ModelID, spec.Name)
			}
			if !strings.Contains(spec.Name, "-tp") && !strings.Contains(spec.Name, "-pp") {
				t.Errorf("%s/%s pins %s cards but its name does not name the topology", model.ModelID, spec.Name, match[1])
			}
		}
	}
	if pins == 0 {
		t.Fatal("no PPU spec pins a card count; the rule would silently stop applying")
	}
}
