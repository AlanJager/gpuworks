package seed

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/placement"
)

const inkling2MiniModel = "thinkingmachines/inkling-2-mini"

// The architecture is only in vLLM nightlies; the recipe's own
// vllm/vllm-openai:inkling-2-mini is a mutable tag with sm100/sm103 kernels
// only, so the specs pin the multi-arch nightly they were measured with.
const inkling2MiniImage = "vllm/vllm-openai:nightly-647a76866b4559a74e27d1c6b55fbffe85c07ce4"

var inkling2MiniSpecs = []string{
	"nvfp4-vllm-nvidia-rtx-5090",
	"nvfp4-vllm-nvidia-hopper-blackwell",
}

const inkling2MiniDSpark = `{"method":"dspark","num_speculative_tokens":7,"draft_sample_method":"probabilistic"}`

func TestInkling2MiniSpecsPinTheNightlyAndTheCheckpoint(t *testing.T) {
	model, _ := catalogSpec(t, inkling2MiniModel, inkling2MiniSpecs[0])
	// The vLLM specs, and the Apple silicon one (inkling2_mini_mlx_test.go).
	if len(model.Specs) != len(inkling2MiniSpecs)+1 {
		t.Fatalf("specs = %d, want %d", len(model.Specs), len(inkling2MiniSpecs)+1)
	}
	if model.ContextLength != 1048576 || !slices.Equal(model.InputModalities, []string{"text", "image"}) {
		t.Errorf("context = %d, modalities = %v", model.ContextLength, model.InputModalities)
	}
	// Measured on the RTX 5090: 7.73 GiB of KV held 87,175 tokens.
	if got := model.KVBytesPerToken["vllm"]; got != 95211 {
		t.Errorf("kv_bytes_per_token.vllm = %d, want 95211", got)
	}
	for _, name := range inkling2MiniSpecs {
		_, spec := catalogSpec(t, inkling2MiniModel, name)
		if spec.Engine != "vllm" || spec.Image != inkling2MiniImage || len(spec.ImageSources) != 0 {
			t.Errorf("%s: engine %q image %q sources %v", name, spec.Engine, spec.Image, spec.ImageSources)
		}
		if spec.Weights.Quantization != "nvfp4" || spec.WeightBytes != 20191643333 || len(spec.Weights.Sources) != 2 {
			t.Errorf("%s: weights = %+v (%d bytes)", name, spec.Weights, spec.WeightBytes)
			continue
		}
		want := [][3]string{
			{"huggingface", "thinkingmachines/Inkling-2-Mini", "b85c3cfa615fc413cc9af3379f491b963f3b586a"},
			{"modelscope", "thinkingmachines/Inkling-2-Mini", "1040c7393f82a68d2b0ae53d241c71e440ac7e17"},
		}
		for i, source := range spec.Weights.Sources {
			if got := [3]string{source.Provider, source.RepoID, source.Revision}; got != want[i] {
				t.Errorf("%s: source %d = %v, want %v", name, i, got, want[i])
			}
		}
		if spec.API == nil || spec.API.Metrics == nil || spec.API.Metrics.Inherit != "vllm" {
			t.Errorf("%s: api.metrics does not inherit the vLLM mapping: %+v", name, spec.API)
		}
	}
}

// RTX 3090 and RTX 4090 refuse the checkpoint's quantization method
// (minimum capability 9.0), so no spec may match Ampere or Ada; the 5090
// spec matches only the card it was measured on, and the recipe-sourced
// spec covers Hopper and data-centre Blackwell.
func TestInkling2MiniSpecsMatchOnlyTheirCards(t *testing.T) {
	cards := []struct {
		name string
		cc   float64
	}{
		{"NVIDIA GeForce RTX 3090", 8.6},
		{"NVIDIA GeForce RTX 4090", 8.9},
		{"NVIDIA A100-SXM4-80GB", 8.0},
		{"NVIDIA L40S", 8.9},
		{"NVIDIA H100 80GB HBM3", 9.0},
		{"NVIDIA H20", 9.0},
		{"NVIDIA B200", 10.0},
		{"NVIDIA B300 SXM6 AC", 10.3},
		{"NVIDIA GeForce RTX 5090", 12.0},
		{"NVIDIA GeForce RTX 5080", 12.0},
		{"NVIDIA RTX PRO 6000 Blackwell Server Edition", 12.0},
		{"NVIDIA GB10", 12.1},
	}
	want := map[string][]string{
		"nvfp4-vllm-nvidia-rtx-5090":         {"NVIDIA GeForce RTX 5090"},
		"nvfp4-vllm-nvidia-hopper-blackwell": {"NVIDIA H100 80GB HBM3", "NVIDIA H20", "NVIDIA B200", "NVIDIA B300 SXM6 AC"},
	}
	for name, matches := range want {
		_, spec := catalogSpec(t, inkling2MiniModel, name)
		// The capability floor is the engine's, so it must hold on the
		// compute capability alone, whatever the name pattern admits.
		for _, cc := range []float64{8.0, 8.6, 8.9} {
			if chipmatch.Evaluate(chipmatch.GPU{Vendor: "nvidia", CC: cc}, chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC}).Matched {
				t.Errorf("%s: cc %q admits compute capability %g", name, spec.GPUs.CC, cc)
			}
		}
		predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, CC: spec.GPUs.CC, Name: spec.GPUs.Name}
		for _, card := range cards {
			got := chipmatch.Evaluate(chipmatch.GPU{Name: card.name, Vendor: "nvidia", CC: card.cc}, predicate)
			if got.Matched != slices.Contains(matches, card.name) {
				t.Errorf("%s on %s: matched = %v (%s)", name, card.name, got.Matched, got.Reason)
			}
		}
	}
}

// Each spec places on exactly one card large enough for its weights and the
// KV cache of one request at its --max-model-len, and launches the command
// measured on the RTX 5090 (or, for the recipe spec, the recipe's command).
// The 5090 spec carries no DSpark: at the deployment's 0.9 share its draft
// cache leaves no room for even a 16K request.
func TestInkling2MiniSpecsPlaceOnOneCardAndLaunchTheMeasuredCommand(t *testing.T) {
	cases := []struct {
		spec           string
		fits, tooSmall float64 // GiB per card
		maxModelLen    string
		dspark         bool
	}{
		{"nvfp4-vllm-nvidia-rtx-5090", 31.84, 28, "65536", false},
		{"nvfp4-vllm-nvidia-hopper-blackwell", 79.6, 40, "262144", true},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			_, spec := catalogSpec(t, inkling2MiniModel, tc.spec)
			if spec.Placement == nil {
				t.Fatal("missing placement contract")
			}
			var accepted placement.Result
			for _, gpu := range []struct {
				count  int
				memory float64
				ok     bool
			}{{1, tc.fits, true}, {2, tc.fits, false}, {4, tc.fits, false}, {1, tc.tooSmall, false}} {
				result, err := placement.Resolve(context.Background(), spec.Placement.Resolver, placement.GPU{Count: gpu.count, Vendor: "nvidia", MinMemoryGiB: gpu.memory})
				if err != nil {
					t.Fatal(err)
				}
				if (result.Reason == "") != gpu.ok {
					t.Fatalf("%d × %g GiB: rejection %q, want accepted=%v", gpu.count, gpu.memory, result.Reason, gpu.ok)
				}
				if gpu.ok {
					accepted = result
				}
			}
			command := kimiK31ProLaunch(t, spec, accepted.Variables, deploy.Params{AssignedGPUs: 1, TensorParallel: 1, NNodes: 1})
			for flag, value := range map[string]string{
				"--max-model-len": tc.maxModelLen, "--tool-call-parser": "inkling", "--reasoning-parser": "inkling",
			} {
				if commandFlagCount(command, flag) != 1 || commandFlagValue(command, flag) != value {
					t.Errorf("want exactly one %s %s in %v", flag, value, command)
				}
			}
			if commandFlagCount(command, "--enable-auto-tool-choice") != 1 {
				t.Errorf("want auto tool choice in %v", command)
			}
			if tc.dspark != (commandFlagCount(command, "--speculative-config") == 1) || (tc.dspark && commandFlagValue(command, "--speculative-config") != inkling2MiniDSpark) {
				t.Errorf("speculative config (want DSpark=%v) in %v", tc.dspark, command)
			}
			// The checkpoint's chat template and the inkling tokenizer are
			// picked by vLLM itself; the Inkling 1 flags must not leak in.
			for _, flag := range []string{"--tokenizer-mode", "--trust-remote-code", "--chat-template"} {
				if commandFlagCount(command, flag) != 0 {
					t.Errorf("unexpected %s in %v", flag, command)
				}
			}
		})
	}
}
