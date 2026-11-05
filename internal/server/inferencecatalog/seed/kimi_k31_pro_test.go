package seed

import (
	"context"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

const kimiK31ProModel = "moonshotai/kimi-k3.1-pro"

// kimiK31ProImages are the day-0 images the upstream recipes name; the
// platform runners predate the kimi_k31 architecture and cannot load it.
var kimiK31ProImages = map[string]map[string]string{
	"sglang": {"nvidia": "lmsysorg/sglang:dev-kimi-k31-pro", "amd": "lmsysorg/sglang:dev-kimi-k31-pro-mi35x"},
	"vllm": {
		"nvidia": "vllm/vllm-openai:kimi-k31", "amd": "vllm/vllm-openai-rocm:kimi-k31",
		"ppu": "egslingjun-registry.cn-wulanchabu.cr.aliyuncs.com/egslingjun-tmp/inference-xpu-pytorch:kimi-k3.1-pro-vllm0.32.0-cuda13.0-pytorch2.13.0-20261103",
	},
}

func TestKimiK31ProSpecsUseTheDayZeroImagesAndPinnedWeights(t *testing.T) {
	model, _ := catalogSpec(t, kimiK31ProModel, "mxfp4-vllm-nvidia-b300-tp8")
	if len(model.Specs) != 12 {
		t.Fatalf("specs = %d, want 12", len(model.Specs))
	}
	for _, spec := range model.Specs {
		if want := kimiK31ProImages[spec.Engine][spec.GPUs.Vendor]; spec.Image != want {
			t.Errorf("%s: image = %q, want %q", spec.Name, spec.Image, want)
		}
		if spec.Weights.Quantization != "mxfp4" || spec.WeightBytes != 1561355966963 || len(spec.Weights.Sources) != 1 {
			t.Errorf("%s: weights = %+v (%d bytes)", spec.Name, spec.Weights, spec.WeightBytes)
			continue
		}
		source := spec.Weights.Sources[0]
		if source.Provider != "huggingface" || source.RepoID != "moonshotai/Kimi-K3.1-Pro" || source.Revision != "6d0a0202956796543364d666ab19c6ad1a494ee1" {
			t.Errorf("%s: source = %+v", spec.Name, source)
		}
		for _, pair := range [][2]string{{"--reasoning-parser", "kimi_k3"}, {"--tool-call-parser", "kimi_k3"}} {
			if !containsArgumentPair(spec.Args, pair[0], pair[1]) {
				t.Errorf("%s: missing %s %s", spec.Name, pair[0], pair[1])
			}
		}
	}
}

func TestKimiK31ProSpecsMatchOnlyTheirCards(t *testing.T) {
	cards := []struct {
		name, vendor string
		cc           float64
	}{
		{"NVIDIA H200", "nvidia", 9.0},
		{"NVIDIA H100 80GB HBM3", "nvidia", 9.0},
		{"NVIDIA H20", "nvidia", 9.0},
		{"NVIDIA B200", "nvidia", 10.0},
		{"NVIDIA B300 SXM6 AC", "nvidia", 10.3},
		{"NVIDIA GB300", "nvidia", 10.3},
		{"AMD Instinct MI355X", "amd", 0},
		{"AMD Instinct MI350X", "amd", 0},
		{"AMD Instinct MI300X", "amd", 0},
		{"PPU-ZW810E", "ppu", 0},
		{"PPU-M890", "ppu", 0},
		{"ZW-M890P", "ppu", 0},
	}
	want := map[string][]string{
		"mxfp4-sglang-nvidia-b200-tp-dcp-multinode": {"NVIDIA B200"},
		"mxfp4-sglang-nvidia-b300-tp8":              {"NVIDIA B300 SXM6 AC"},
		"mxfp4-sglang-nvidia-b300-tp8-dcp8":         {"NVIDIA B300 SXM6 AC"},
		"mxfp4-sglang-amd-mi355x-tp8":               {"AMD Instinct MI355X", "AMD Instinct MI350X"},
		"mxfp4-sglang-amd-mi355x-tp8-dcp8":          {"AMD Instinct MI355X", "AMD Instinct MI350X"},
		"mxfp4-vllm-nvidia-b300-tp8":                {"NVIDIA B300 SXM6 AC"},
		"mxfp4-vllm-nvidia-b300-tep8":               {"NVIDIA B300 SXM6 AC"},
		"mxfp4-vllm-nvidia-b300-tp8-dcp8":           {"NVIDIA B300 SXM6 AC"},
		"mxfp4-vllm-nvidia-gb300-tp8-multinode":     {"NVIDIA GB300"},
		"mxfp4-vllm-amd-mi355x-tp8":                 {"AMD Instinct MI355X"},
		"mxfp4-vllm-ppu-810e-tp32-ep-multinode":     {"PPU-ZW810E"},
		"mxfp4-vllm-ppu-m890-tp8-dp-multinode":      {"PPU-M890"},
	}
	for specName, matches := range want {
		_, spec := catalogSpec(t, kimiK31ProModel, specName)
		predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
		for _, card := range cards {
			expected := false
			for _, name := range matches {
				expected = expected || name == card.name
			}
			got := chipmatch.Evaluate(chipmatch.GPU{Name: card.name, Vendor: card.vendor, CC: card.cc}, predicate)
			if got.Matched != expected {
				t.Errorf("%s on %s: matched = %v, want %v (%s)", specName, card.name, got.Matched, expected, got.Reason)
			}
		}
	}
}

// On a 275 GB B300 the vLLM recipe's command does not start: one 1M-token
// request needs 16.92 GiB of KV per card where 14.21 GiB is left, and vLLM's
// default of 1024 running sequences exceeds the 839 Kimi Delta Attention state
// blocks that fit. The NVIDIA and AMD vLLM profiles therefore cap both, and
// the cap reaches the command exactly once.
func TestKimiK31ProVLLMProfilesCapContextAndRunningSequences(t *testing.T) {
	launches := map[string]deploy.Params{
		"mxfp4-vllm-nvidia-b300-tp8":            {AssignedGPUs: 8, TensorParallel: 8, NNodes: 1},
		"mxfp4-vllm-nvidia-b300-tep8":           {AssignedGPUs: 8, TensorParallel: 8, NNodes: 1},
		"mxfp4-vllm-nvidia-b300-tp8-dcp8":       {AssignedGPUs: 8, TensorParallel: 8, NNodes: 1},
		"mxfp4-vllm-amd-mi355x-tp8":             {AssignedGPUs: 8, TensorParallel: 8, NNodes: 1},
		"mxfp4-vllm-nvidia-gb300-tp8-multinode": {AssignedGPUs: 4, TensorParallel: 8, NNodes: 2, MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0.1"},
	}
	for name, params := range launches {
		_, spec := catalogSpec(t, kimiK31ProModel, name)
		var variables map[string]any
		if spec.Placement != nil {
			result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: params.AssignedGPUs, Vendor: spec.GPUs.Vendor, MinMemoryGiB: 268})
			if err != nil || result.Reason != "" {
				t.Fatalf("%s: placement on 8 × 268 GiB refused (%v, %q)", name, err, result.Reason)
			}
			variables = result.Variables
		}
		command := kimiK31ProLaunch(t, spec, variables, params)
		for flag, value := range map[string]string{"--max-model-len": "262144", "--max-num-seqs": "128"} {
			if commandFlagCount(command, flag) != 1 || commandFlagValue(command, flag) != value {
				t.Errorf("%s: want exactly one %s %s in %v", name, flag, value, command)
			}
		}
	}
}

// Hopper is not offered: with the current images SGLang runs out of memory
// while loading and vLLM stalls in the Triton fallback of attn_res.
func TestKimiK31ProOffersNoHopperProfile(t *testing.T) {
	model, _ := catalogSpec(t, kimiK31ProModel, "mxfp4-vllm-nvidia-b300-tp8")
	for _, spec := range model.Specs {
		predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
		for _, card := range []string{"NVIDIA H200", "NVIDIA H100 80GB HBM3", "NVIDIA H20"} {
			if chipmatch.Evaluate(chipmatch.GPU{Name: card, Vendor: "nvidia", CC: 9.0}, predicate).Matched {
				t.Errorf("%s matches %s", spec.Name, card)
			}
		}
	}
	// Measured at TP8: 33.51 GiB of KV per card held 1,972,777 tokens.
	if got := model.KVBytesPerToken["vllm"]; got != 145920 {
		t.Errorf("kv_bytes_per_token.vllm = %d, want 145920", got)
	}
}

func TestKimiK31ProB200SpecSizesDecodeContextParallelToTP(t *testing.T) {
	_, spec := catalogSpec(t, kimiK31ProModel, "mxfp4-sglang-nvidia-b200-tp-dcp-multinode")
	command := kimiK31ProLaunch(t, spec, nil, deploy.Params{
		AssignedGPUs: 8, TensorParallel: 16, NNodes: 2, MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0.1",
	})
	if commandFlagValue(command, "--tp-size") != "16" || commandFlagValue(command, "--dcp-size") != "16" {
		t.Fatalf("want TP16 · DCP16, got %v", command)
	}
}

// The vLLM recipe's PPU layouts: TP32 with expert parallelism over two
// 16-PPU 810E nodes, and TP8 × DP4 with expert parallelism over four 8-PPU
// M890 nodes. The M890 template states TP8 per node; the deployment states
// one data-parallel replica per node, so TP 32 and DP 4 over four nodes
// launch TP8 × DP4, and leaving DP out is refused rather than run on 8 cards.
func TestKimiK31ProPPUSpecsLaunchTheRecipeTopologies(t *testing.T) {
	multiNode := deploy.Params{MasterAddr: "10.0.0.1", MasterPort: 29500, NodeAddress: "10.0.0.2"}
	_, e810 := catalogSpec(t, kimiK31ProModel, "mxfp4-vllm-ppu-810e-tp32-ep-multinode")
	params := multiNode
	params.AssignedGPUs, params.TensorParallel, params.NNodes = 16, 32, 2
	command := kimiK31ProLaunch(t, e810, nil, params)
	for flag, value := range map[string]string{
		"--tensor-parallel-size": "32", "--nnodes": "2", "--moe-backend": "ppu_deep_gemm_w4a16", "--max-num-seqs": "256",
	} {
		if commandFlagCount(command, flag) != 1 || commandFlagValue(command, flag) != value {
			t.Errorf("810E: want exactly one %s %s in %v", flag, value, command)
		}
	}
	if commandFlagCount(command, "--enable-expert-parallel") != 1 {
		t.Errorf("810E: want expert parallelism in %v", command)
	}

	_, m890 := catalogSpec(t, kimiK31ProModel, "mxfp4-vllm-ppu-m890-tp8-dp-multinode")
	for rank := 0; rank < 4; rank++ {
		params := multiNode
		params.AssignedGPUs, params.TensorParallel, params.NNodes, params.NodeRank, params.Headless = 8, 32, 4, rank, rank > 0
		params.Quick.DP = 4
		command := kimiK31ProLaunch(t, m890, nil, params)
		for flag, value := range map[string]string{
			"--tensor-parallel-size": "8", "--data-parallel-size": "4", "--nnodes": "4", "--node-rank": strconv.Itoa(rank),
			"--all2all-backend": "deepep_low_latency",
		} {
			if commandFlagCount(command, flag) != 1 || commandFlagValue(command, flag) != value {
				t.Errorf("M890 rank %d: want exactly one %s %s in %v", rank, flag, value, command)
			}
		}
	}
	params = multiNode
	params.AssignedGPUs, params.TensorParallel, params.NNodes = 8, 32, 4
	params.ModelPath, params.APIPort, params.TemplateArgs, params.TemplateEnv = "/model", 30000, m890.Args, m890.Env
	engine, err := deploy.SelectEngine(m890.Engine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{Engine: engine, Image: m890.Image, Params: params, APIContract: m890.API}); err == nil {
		t.Fatal("M890 TP8 launched on four nodes without a data-parallel degree")
	}
	for _, count := range []int{4, 16} {
		result, err := placement.Resolve(context.Background(), m890.Placement.Resolver, placement.GPU{Count: count, Vendor: "ppu", MinMemoryGiB: 144})
		if err != nil || result.Reason == "" {
			t.Errorf("M890 accepted %d PPUs per node (err %v)", count, err)
		}
	}
}

// Single-node recipes either pin the eight GPUs they were tuned on or, for
// plain TP, accept the fewest cards whose memory holds the weights.
func TestKimiK31ProSingleNodeSpecsPlaceAndLaunch(t *testing.T) {
	cases := []struct {
		spec, vendor string
		memoryGiB    float64
		accepts      []int
		flags        map[string]string
	}{
		{"mxfp4-sglang-nvidia-b300-tp8", "nvidia", 268, []int{8}, map[string]string{"--tp-size": "8"}},
		{"mxfp4-sglang-nvidia-b300-tp8-dcp8", "nvidia", 268, []int{8}, map[string]string{"--tp-size": "8", "--dcp-size": "8", "--ep-size": "2"}},
		{"mxfp4-sglang-amd-mi355x-tp8", "amd", 288, []int{8}, map[string]string{"--tp-size": "8", "--max-running-requests": "4"}},
		{"mxfp4-sglang-amd-mi355x-tp8-dcp8", "amd", 288, []int{8}, map[string]string{"--tp-size": "8", "--dcp-size": "8"}},
		{"mxfp4-vllm-nvidia-b300-tp8", "nvidia", 268, []int{8}, map[string]string{"--tensor-parallel-size": "8", "--load-format": "fastsafetensors"}},
		{"mxfp4-vllm-nvidia-b300-tep8", "nvidia", 268, []int{8}, map[string]string{"--tensor-parallel-size": "8", "--moe-backend": "deep_gemm_mega_moe"}},
		{"mxfp4-vllm-nvidia-b300-tp8-dcp8", "nvidia", 268, []int{8}, map[string]string{"--tensor-parallel-size": "8", "--decode-context-parallel-size": "8"}},
		{"mxfp4-vllm-amd-mi355x-tp8", "amd", 288, []int{8}, map[string]string{"--tensor-parallel-size": "8"}},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			_, spec := catalogSpec(t, kimiK31ProModel, tc.spec)
			if spec.Placement == nil {
				t.Fatal("missing placement contract")
			}
			var accepted placement.Result
			for _, count := range []int{1, 2, 4, 6, 8} {
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{
					Count: count, Vendor: tc.vendor, MinMemoryGiB: tc.memoryGiB,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := false
				for _, ok := range tc.accepts {
					want = want || ok == count
				}
				if (result.Reason == "") != want {
					t.Fatalf("%d × %g GiB: rejection %q, want accepted=%v", count, tc.memoryGiB, result.Reason, want)
				}
				if want {
					accepted = result
				}
			}
			command := kimiK31ProLaunch(t, spec, accepted.Variables, deploy.Params{AssignedGPUs: 8, TensorParallel: 8, NNodes: 1})
			for flag, value := range tc.flags {
				if commandFlagCount(command, flag) != 1 || commandFlagValue(command, flag) != value {
					t.Errorf("want exactly one %s %s in %v", flag, value, command)
				}
			}
		})
	}
}

func kimiK31ProLaunch(t *testing.T, spec inferencecatalog.InferenceSpec, variables map[string]any, params deploy.Params) []string {
	t.Helper()
	engine, err := deploy.SelectEngine(spec.Engine)
	if err != nil {
		t.Fatal(err)
	}
	params.ModelPath, params.APIPort, params.TemplateArgs, params.TemplateEnv = "/model", 30000, spec.Args, spec.Env
	if params.Headless {
		params.APIPort = 0
	}
	launch, err := deploy.ResolveLaunch(deploy.ResolveLaunchInput{
		Engine: engine, Image: spec.Image, Params: params, PlacementVariables: variables, APIContract: spec.API,
	})
	if err != nil {
		t.Fatalf("%s: launch refused: %v", spec.Name, err)
	}
	return append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
}
