package deploy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Presence in Raw is what makes a field explicit, and an empty string is a
// value like any other: it is how the form says "clear what the template asked
// for". Treating it as absence would silently restore the template's value and
// leave the user unable to turn a parser off at all.
func TestMaterializeTreatsAnExplicitEmptyStringAsAChoice(t *testing.T) {
	template := []string{"--reasoning-parser", "qwen3", "--tool-call-parser", "hermes"}
	out, err := Materialize(MaterializeInput{
		Engine:       EngineVLLM,
		Raw:          map[string]any{"reasoning_parser": "", "tool_call_parser": ""},
		TemplateArgs: template,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.ReasoningParser != "" || out.Quick.ToolCallParser != "" {
		t.Fatalf("cleared parsers came back set: %+v", out.Quick)
	}
	if err := ValidateLaunchConfigForEngine(EngineVLLM, out.Quick); err != nil {
		t.Fatalf("a cleared parser is a valid configuration: %v", err)
	}
	// The template still reports what it asked for, which is what makes this a
	// deviation the form can show rather than a silent difference.
	if out.TemplateParams["reasoning_parser"] != "qwen3" || out.TemplateParams["tool_call_parser"] != "hermes" {
		t.Fatalf("template_params=%v", out.TemplateParams)
	}
	if len(out.TemplateResidual) != 0 {
		t.Fatalf("both flags are dictionary keys and must not remain residual: %v", out.TemplateResidual)
	}

	built := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: out.Quick, TemplateArgs: out.TemplateResidual})
	argv := strings.Join(append(append([]string(nil), built.Entrypoint...), built.Argv...), " ")
	for _, flag := range []string{"--reasoning-parser", "--tool-call-parser"} {
		if strings.Contains(argv, flag) {
			t.Fatalf("%s reached the engine after being cleared: %s", flag, argv)
		}
	}

	// Leaving the keys out entirely is the other case: the template's values win.
	kept, err := Materialize(MaterializeInput{Engine: EngineVLLM, TemplateArgs: template})
	if err != nil {
		t.Fatal(err)
	}
	if kept.Quick.ReasoningParser != "qwen3" || kept.Quick.ToolCallParser != "hermes" {
		t.Fatalf("an absent key must keep the template's value: %+v", kept.Quick)
	}
}

// A user residual token that repeats a template residual token exactly is not a
// duplicate on the command line: the user's copy wins and the template's is
// dropped, so the engine sees the flag once.
func TestResidualLayersDeduplicateAUserTokenThatRepeatsTheTemplate(t *testing.T) {
	built := BuildVLLM(Params{
		ModelPath: "/model", APIPort: 30000, TensorParallel: 1,
		TemplateArgs: []string{"--load-format", "fastsafetensors", "--moe-backend", "marlin"},
		ExtraArgs:    []string{"--load-format", "fastsafetensors"},
	})
	argv := strings.Join(built.Argv, " ")
	if got := strings.Count(argv, "--load-format"); got != 1 {
		t.Fatalf("--load-format appears %d times: %v", got, built.Argv)
	}
	if !strings.Contains(argv, "--load-format fastsafetensors") || !strings.Contains(argv, "--moe-backend marlin") {
		t.Fatalf("argv lost a residual argument: %v", built.Argv)
	}
}

// Upstream recipes and API clients spell an enum however their own docs do.
// Case is not meaning: the value is recognized and stored in the dictionary's
// spelling, so the estimator sees it and everything downstream compares one
// string. The dictionary spells SGLang's algorithms as SGLang does (EAGLE3),
// so a lower-case request is folded up. A value that is not an option at all
// is still not decoded.
func TestEnumValuesAreMatchedCaseInsensitivelyAndCanonicalized(t *testing.T) {
	params, residual, err := ReverseArgs(EngineSGLang, []string{"--speculative-algorithm", "eagle3"})
	if err != nil {
		t.Fatal(err)
	}
	if params["speculative_algorithm"] != "EAGLE3" || len(residual) != 0 {
		t.Fatalf("params=%v residual=%v, want the canonical EAGLE3", params, residual)
	}
	vllm, residual, err := ReverseArgs(EngineVLLM, []string{"--dtype", "BFloat16", "--kv-cache-dtype", "FP8"})
	if err != nil {
		t.Fatal(err)
	}
	if vllm["dtype"] != "bfloat16" || vllm["kv_cache_dtype"] != "fp8" || len(residual) != 0 {
		t.Fatalf("params=%v residual=%v", vllm, residual)
	}

	out, err := Materialize(MaterializeInput{Engine: EngineSGLang, Raw: map[string]any{"speculative_algorithm": "eagle3"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.SpeculativeAlgorithm != "EAGLE3" {
		t.Fatalf("speculative_algorithm=%q, want the canonical EAGLE3", out.Quick.SpeculativeAlgorithm)
	}
	if err := ValidateLaunchConfigForEngine(EngineSGLang, out.Quick); err != nil {
		t.Fatalf("a canonicalized enum must validate: %v", err)
	}

	// A spelling that is not an option in any case stays a passthrough token,
	// and as a form field it is rejected rather than silently accepted.
	unknown, residual, err := ReverseArgs(EngineSGLang, []string{"--speculative-algorithm", "eagle9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 || !reflect.DeepEqual(residual, []string{"--speculative-algorithm", "eagle9"}) {
		t.Fatalf("params=%v residual=%v", unknown, residual)
	}
	bad, err := Materialize(MaterializeInput{Engine: EngineSGLang, Raw: map[string]any{"speculative_algorithm": "eagle9"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateLaunchConfigForEngine(EngineSGLang, bad.Quick); err == nil {
		t.Fatal("eagle9 is not an option and must not validate")
	}
}

// Zero is a choice for a parameter that has a default, not an omission. A
// request that states gpu_memory_utilization: 0 has to be told so here, at the
// write boundary, rather than reaching the scheduler as a request nobody could
// have sent.
// A stated zero is a choice, and the only place that can tell a stated zero
// from an absent key is the write boundary, where the request still says which
// keys it carried. Materialize rejects it there; by the time a LaunchConfig
// exists, a zero can only mean "nobody stated this".
func TestZeroIsValidatedForAParameterThatHasADefault(t *testing.T) {
	// The bound comes from the dictionary rather than being spelled out here:
	// what this case is about is that the refusal names the row's own minimum,
	// which a number copied into the test would stop proving the day the
	// dictionary moved the bound.
	dictionary := engineParams(EngineVLLM)
	for _, key := range []string{"gpu_memory_utilization", "kv_offload_read_threads", "kv_offload_write_threads"} {
		t.Run(key, func(t *testing.T) {
			spec, ok := dictionary[key]
			if !ok || spec.Min == nil {
				t.Fatalf("%s has no dictionary minimum to report; this case needs a key that has one", key)
			}
			wantBound := fmt.Sprintf("%v", *spec.Min)
			_, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{key: 0}})
			if err == nil || !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), wantBound) {
				t.Fatalf("err=%v, want %s and its dictionary minimum %s named", err, key, wantBound)
			}
		})
	}
	// The same key left unstated is not an error: the thread counts ride with
	// the disk tier and stay unset while it is off, and the utilization states
	// the dictionary default instead.
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"max_model_len": 4096}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.GPUMemUtil != 0.9 || out.Quick.KVOffloadReadThreads != 0 {
		t.Fatalf("unstated keys must resolve to their tier-aware defaults: %+v", out.Quick)
	}
	if err := ValidateLaunchConfigForEngine(EngineVLLM, out.Quick); err != nil {
		t.Fatalf("a configuration with defaulted keys must validate: %v", err)
	}
}

// The offload thread counts are folded into one composite flag, which has no
// way to say "unset" for a single member. An untouched disk-offload deployment
// therefore gets the dictionary's 16, read from the table rather than written
// out as a literal in the connector JSON.
func TestDiskKVTransferConfigTakesItsThreadsFromTheDictionary(t *testing.T) {
	quick := materializedConfig(t, EngineVLLM)
	quick.KVDiskOffload, quick.KVOffloadingSize, quick.SharedKVDir = true, 4, "/cache/kv"
	if quick.KVOffloadReadThreads != 0 || quick.KVOffloadWriteThreads != 0 {
		t.Fatalf("thread counts nobody stated must be unset, got %d/%d", quick.KVOffloadReadThreads, quick.KVOffloadWriteThreads)
	}
	built := BuildVLLM(Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, Quick: quick})
	var raw string
	for index := range built.Argv {
		if built.Argv[index] == "--kv-transfer-config" && index+1 < len(built.Argv) {
			raw = built.Argv[index+1]
		}
	}
	if raw == "" {
		t.Fatalf("missing disk KV config: %v", built.Argv)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	extra, _ := config["kv_connector_extra_config"].(map[string]any)
	tiers, _ := extra["secondary_tiers"].([]any)
	if len(tiers) != 1 {
		t.Fatalf("secondary tiers = %#v", extra["secondary_tiers"])
	}
	tier, _ := tiers[0].(map[string]any)
	if tier["n_read_threads"] != float64(16) || tier["n_write_threads"] != float64(16) {
		t.Fatalf("fs tier = %#v, want 16 read and 16 write threads", tier)
	}
}
