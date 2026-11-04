package deploy

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestResolvedFeaturesUseEffectiveExecutableAndExactDialect(t *testing.T) {
	for _, tc := range []struct {
		name, engine     string
		entry, args      []string
		pp, sleep, known bool
	}{
		{"vllm short", EngineVLLM, []string{"vllm", "serve"}, []string{"-pp", "2"}, true, false, true},
		{"wrong vllm alias", EngineVLLM, []string{"vllm", "serve"}, []string{"--pp", "2"}, false, false, false},
		{"sglang alias", EngineSGLang, []string{"python3", "-m", "sglang.launch_server"}, []string{"--pp", "2"}, true, false, true},
		{"last sleep wins", EngineVLLM, []string{"vllm", "serve"}, []string{"--enable-sleep-mode", "--enable-sleep-mode=false"}, false, false, true},
		{"quoted flag value", EngineVLLM, []string{"vllm", "serve"}, []string{"--served-model-name=--enable-sleep-mode"}, false, false, true},
		{"other subcommand", EngineVLLM, []string{"vllm", "bench"}, []string{"-pp", "2"}, false, false, false},
		{"other module", EngineVLLM, []string{"python3", "-m", "vllm.other"}, []string{"-pp", "2"}, false, false, false},
		{"fake python prefix", EngineVLLM, []string{"python-wrapper", "-m", "vllm.entrypoints.openai.api_server"}, []string{"-pp", "2"}, false, false, false},
		{"invalid numeric", EngineVLLM, []string{"vllm", "serve"}, []string{"-pp", "invalid"}, false, false, false},
		{"invalid kv config", EngineVLLM, []string{"vllm", "serve"}, []string{"--kv-transfer-config", "{"}, false, false, false},
		{"unknown command", EngineVLLM, []string{"my-launcher"}, []string{"-pp", "2"}, false, false, false},
		{"shell unknown", EngineVLLM, []string{"/bin/sh"}, []string{"-c", "vllm serve -pp 2"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ResolveRequestedFeatures(tc.engine, ResolvedLaunch{Entrypoint: tc.entry, Argv: tc.args}, Params{})
			if f.CommandKnown != tc.known || f.PipelineParallel != tc.pp || f.EngineSleep != tc.sleep {
				t.Fatalf("features=%+v", f)
			}
		})
	}
}
func TestResolvedSnapshotAppliesAllVendorPolicies(t *testing.T) {
	launch := ResolvedLaunch{Entrypoint: []string{"vllm", "serve"}, Argv: []string{"--enable-sleep-mode"}}
	for _, vendor := range []string{"nvidia", "ascend", "amd", "apple"} {
		features := RequestedLaunchFeatures(EngineVLLM, launch, Params{Vendor: vendor})
		if len(features) != 1 || features[0] != accelerator.FeatureEngineSleep {
			t.Fatalf("%s features=%v", vendor, features)
		}
		err := ValidateTargetFeatures(EngineVLLM, launch, Params{Vendor: vendor})
		if (vendor == "apple") != (err != nil) {
			t.Fatalf("%s error=%v", vendor, err)
		}
	}
	// Data parallelism is the feature AMD still lacks.
	dp := ResolvedLaunch{Entrypoint: []string{"vllm", "serve"}, Argv: []string{"--data-parallel-size", "2"}}
	for _, vendor := range []string{"nvidia", "amd"} {
		err := ValidateTargetFeatures(EngineVLLM, dp, Params{Vendor: vendor})
		if (vendor == "amd") != (err != nil) {
			t.Fatalf("%s data-parallel error=%v", vendor, err)
		}
	}
}
func TestPartitionCapabilityHasNoFeatureAuthority(t *testing.T) {
	for _, vendor := range []string{"amd", "apple", "nvidia"} {
		if got := RequestedLaunchFeatures(EngineVLLM, ResolvedLaunch{}, Params{Vendor: vendor, VRAMLimitBytes: 1, PartitionBackend: "explicit"}); len(got) != 0 {
			t.Fatalf("partition leaked into feature policy: %v", got)
		}
	}
}
// AMD requires explicit device selection and owns a memory-only logical
// partition backend; ValidateTargetLaunch must admit its own VRAM-only
// quota (assuming the assigned GPU requirement is met) but structurally
// refuse any non-default SM limit, since AMD exposes no compute control.
// Other vendors are unaffected: PPU and Hygon do expose an SM limit.
func TestValidateTargetLaunchRefusesAMDComputeLimit(t *testing.T) {
	launch := ResolvedLaunch{Entrypoint: []string{"vllm", "serve"}}
	base := Params{Vendor: "amd", AssignedGPUs: 1, VRAMLimitBytes: 1 << 30, PartitionBackend: "amd-dgpu"}
	if err := ValidateTargetLaunch(EngineVLLM, launch, base); err != nil {
		t.Fatalf("AMD's own memory-only partition contract was refused: %v", err)
	}
	limited := base
	limited.SMLimitPct = 50
	err := ValidateTargetLaunch(EngineVLLM, launch, limited)
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorAcceleratorSMLimitUnsupported {
		t.Fatalf("AMD compute/SM limit error = %v, want %s", err, messages.ErrorAcceleratorSMLimitUnsupported)
	}
	for _, vendor := range []string{"nvidia", "ppu", "hygon"} {
		if err := ValidateTargetLaunch(EngineVLLM, launch, Params{Vendor: vendor, SMLimitPct: 50}); err != nil {
			t.Fatalf("%s SM limit was unexpectedly refused: %v", vendor, err)
		}
	}
}

func TestPreparationHooksCannotSupplyLaunchFeatures(t *testing.T) {
	f := ResolveRequestedFeatures(EngineVLLM, ResolvedLaunch{Entrypoint: []string{"my-launcher"}, PreStartArgv: []string{"vllm", "serve", "-pp", "2"}, PreStartScript: "vllm serve --enable-sleep-mode"}, Params{})
	if f.CommandKnown || f.EngineSleep || f.PipelineParallel {
		t.Fatalf("preparation hook interpreted as launch: %+v", f)
	}
}

func TestUnknownExecutableRequiresProofOnlyForPartialLaunchRestrictions(t *testing.T) {
	launch := ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", "exec custom-server"}}
	for _, vendor := range []string{"amd", "nvidia", "ascend", "apple"} {
		err := ValidateTargetFeatures(EngineVLLM, launch, Params{Vendor: vendor})
		if (vendor == "amd") != (err != nil) {
			t.Fatalf("%s unknown launch error=%v", vendor, err)
		}
	}
}
