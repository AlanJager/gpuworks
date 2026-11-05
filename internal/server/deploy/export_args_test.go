package deploy

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestExportLaunchArgsWritesTheParametersTheEngineEmits(t *testing.T) {
	yes, no := true, false
	served := "public-name"
	quick := LaunchConfig{
		MaxModelLen:          32768,
		GPUMemUtil:           0.9,
		Dtype:                "auto", // the adapter says nothing for auto
		EnablePrefixCaching:  &yes,
		EnableChunkedPrefill: &no,
		ReasoningParser:      "qwen3",
		TrustRemote:          true,
		ServedModelName:      &served,
		CPUCores:             8,
	}
	got, err := ExportLaunchArgs(EngineVLLM, quick, []string{"--tensor-parallel-size", "${TP_SIZE}", "--upstream-only"}, []string{"--seed", "7"})
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"--dtype", "--served-model-name", "--port", "--host", "/model"} {
		if slices.Contains(got.Args, unwanted) {
			t.Fatalf("args carry platform or unstated token %q: %q", unwanted, got.Args)
		}
	}
	for _, pair := range [][2]string{{"--max-model-len", "32768"}, {"--gpu-memory-utilization", "0.9"}, {"--reasoning-parser", "qwen3"}, {"--seed", "7"}, {"--tensor-parallel-size", "${TP_SIZE}"}} {
		at := slices.Index(got.Args, pair[0])
		if at < 0 || at+1 >= len(got.Args) || got.Args[at+1] != pair[1] {
			t.Fatalf("args lack %s %s: %q", pair[0], pair[1], got.Args)
		}
	}
	for _, flag := range []string{"--enable-prefix-caching", "--no-enable-chunked-prefill", "--trust-remote-code", "--upstream-only"} {
		if !slices.Contains(got.Args, flag) {
			t.Fatalf("args lack %s: %q", flag, got.Args)
		}
	}
	// The residual layers keep their order at the end, user tokens last.
	if tail := got.Args[len(got.Args)-5:]; !reflect.DeepEqual(tail, []string{"--tensor-parallel-size", "${TP_SIZE}", "--upstream-only", "--seed", "7"}) {
		t.Fatalf("residual tail = %q", tail)
	}
	if !reflect.DeepEqual(got.Dropped, []string{"cpu_cores"}) {
		t.Fatalf("dropped = %q, want [cpu_cores]", got.Dropped)
	}
}

func TestExportLaunchArgsKeepsComposedFlags(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		quick  LaunchConfig
		want   []string
	}{
		{"vllm speculative config", EngineVLLM, LaunchConfig{SpeculativeAlgorithm: "eagle3", SpeculativeDraftModel: "/draft", SpeculativeTokens: 3}, []string{"--speculative-config"}},
		{"vllm optimization level", EngineVLLM, LaunchConfig{OptimizationLevel: "O3"}, []string{"-O3"}},
		{"vllm cpu offloading", EngineVLLM, LaunchConfig{KVOffloadingSize: 16}, []string{"--kv-offloading-size", "--kv-offloading-backend"}},
		{"sglang speculative", EngineSGLang, LaunchConfig{SpeculativeAlgorithm: "EAGLE3", SpeculativeDraftModel: "/draft", SpeculativeNumSteps: 3, SpeculativeEagleTopK: 1, SpeculativeTokens: 4}, []string{"--speculative-algorithm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ExportLaunchArgs(tc.engine, tc.quick, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, flag := range tc.want {
				if !slices.Contains(got.Args, flag) {
					t.Fatalf("args lack %s: %q", flag, got.Args)
				}
			}
			if len(got.Dropped) != 0 {
				t.Fatalf("dropped = %q, want none", got.Dropped)
			}
		})
	}
}

func TestExportLaunchArgsUserResidualBeatsTemplateResidual(t *testing.T) {
	got, err := ExportLaunchArgs(EngineSGLang, LaunchConfig{MaxNumSeqs: 16}, []string{"--chunked-prefill-size", "4096", "--keep"}, []string{"--chunked-prefill-size", "8192"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got.Args, "4096") || !slices.Contains(got.Args, "8192") || !slices.Contains(got.Args, "--keep") {
		t.Fatalf("args = %q: the user's value replaces the template's, other template tokens stay", got.Args)
	}
}

func TestExportLaunchArgsRoundTripsThroughMaterialize(t *testing.T) {
	yes := true
	quick := LaunchConfig{MaxModelLen: 8192, MaxNumSeqs: 32, KVCacheDtype: "fp8", EnablePrefixCaching: &yes, ToolCallParser: "hermes", EnableAutoToolChoice: true}
	for _, engine := range []string{EngineVLLM, EngineSGLang} {
		t.Run(engine, func(t *testing.T) {
			got, err := ExportLaunchArgs(engine, quick, []string{"--upstream-only"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Materialize(MaterializeInput{Engine: engine, TemplateArgs: got.Args, Base: &LaunchConfig{}})
			if err != nil {
				t.Fatal(err)
			}
			want := exportBuild(mustEngine(t, engine), quick, []string{"--upstream-only"}, nil)
			rebuilt := exportBuild(mustEngine(t, engine), again.Quick, again.TemplateResidual, nil)
			if !slices.Equal(flagValues(rebuilt), flagValues(want)) {
				t.Fatalf("rebuilt command %q, want %q", rebuilt, want)
			}
		})
	}
}

func TestExportLaunchArgsCallableCarriesTheTemplateCommandOnly(t *testing.T) {
	template := []string{"--port", "${API_PORT}", "--model", "${MODEL_PATH}"}
	got, err := ExportLaunchArgs(EngineCallable, LaunchConfig{}, template, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, template) {
		t.Fatalf("args = %q, want the template command %q", got.Args, template)
	}
}

func TestExportLaunchArgsRejectsAnUnknownEngine(t *testing.T) {
	if _, err := ExportLaunchArgs("no-such-engine", LaunchConfig{}, nil, nil); err == nil {
		t.Fatal("unknown engine exported")
	}
}

func TestNotExportableIsAProductError(t *testing.T) {
	var product *messages.ProductError
	if err := notExportable(EngineVLLM, "reason"); !errors.As(err, &product) || product.Code() != ErrorLaunchNotExportable || product.Params()["engine"] != EngineVLLM {
		t.Fatalf("error = %#v", err)
	}
}

func TestFlagValuesIgnoresOrderButNotValues(t *testing.T) {
	a := flagValues([]string{"pos", "--a", "1", "--b", "--c=2", "-O3"})
	b := flagValues([]string{"pos", "-O3", "--c=2", "--b", "--a", "1"})
	if !slices.Equal(a, b) {
		t.Fatalf("same command in another order differs: %q vs %q", a, b)
	}
	if slices.Equal(a, flagValues([]string{"pos", "--a", "2", "--b", "--c=2", "-O3"})) {
		t.Fatal("a changed value compares equal")
	}
	if !slices.Equal(flagValues([]string{"--offset", "-1"}), []string{"--offset\x00-1"}) {
		t.Fatalf("negative number value = %q", flagValues([]string{"--offset", "-1"}))
	}
}
