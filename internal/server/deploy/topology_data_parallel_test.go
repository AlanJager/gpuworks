package deploy

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// dataParallelCase is one command judged against the GPUs placement assigned.
// An accepted case states neither wantCode nor wantErr; a product error states
// its code and the params the form interpolates; a plain error (the tp × pp
// rule as it always was, or a malformed value) states a message fragment.
type dataParallelCase struct {
	name       string
	args       []string
	assigned   int
	nodes      int
	wantCode   string
	wantParams map[string]any
	wantErr    string
}

func runDataParallelCases(t *testing.T, engine Engine, cases []dataParallelCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			launch := ResolvedLaunch{Entrypoint: []string{"serve"}, Argv: test.args}
			err := hooks(engine).ValidateTopology(launch, Params{AssignedGPUs: test.assigned, NNodes: test.nodes})
			switch {
			case test.wantCode != "":
				var productErr *messages.ProductError
				if !errors.As(err, &productErr) || productErr.Code() != test.wantCode {
					t.Fatalf("ValidateTopology() = %v, want product error %s", err, test.wantCode)
				}
				for key, want := range test.wantParams {
					if got := productErr.Params()[key]; fmt.Sprint(got) != fmt.Sprint(want) {
						t.Fatalf("params[%q] = %v, want %v (all: %v)", key, got, want, productErr.Params())
					}
				}
			case test.wantErr != "":
				var productErr *messages.ProductError
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || errors.As(err, &productErr) {
					t.Fatalf("ValidateTopology() = %v, want plain error containing %q", err, test.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("ValidateTopology() = %v, want accepted", err)
				}
			}
		})
	}
}

// vLLM gives every data-parallel replica its own tensor × pipeline GPUs, so
// the world it starts is tp × pp × dp. A degree of one, stated or not, leaves
// the tp × pp rule exactly as it was.
func TestVLLMDataParallelMultipliesTheWorld(t *testing.T) {
	runDataParallelCases(t, vLLMEngine{}, []dataParallelCase{
		{name: "unset degree keeps tp × pp", args: []string{"--tensor-parallel-size", "8"}, assigned: 8},
		{name: "degree one keeps tp × pp", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size", "1"}, assigned: 8},
		{name: "degree one keeps the tp × pp error", args: []string{"--tensor-parallel-size", "4", "--data-parallel-size", "1"}, assigned: 8,
			wantErr: "tensor parallel 4 × pipeline parallel 1 requires 4 GPUs, but placement assigned 8"},
		{name: "injected TP covering the world leaves no GPU for a second replica", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"key": "data_parallel_size", "flag": "--data-parallel-size", "dp": 2, "tp": 8, "pp": 1, "required": 16, "assigned": 8}},
		{name: "tp × dp equal to the world", args: []string{"--tensor-parallel-size", "4", "--data-parallel-size", "2"}, assigned: 8},
		{name: "tp × pp × dp equal to the world", args: []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2", "--data-parallel-size", "2"}, assigned: 8},
		{name: "tp × pp × dp above the world", args: []string{"--tensor-parallel-size", "2", "--pipeline-parallel-size", "2", "--data-parallel-size", "2"}, assigned: 4,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"required": 8, "assigned": 4, "pp": 2}},
		{name: "tp × dp below the world", args: []string{"--tensor-parallel-size", "2", "--data-parallel-size", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"required": 4, "assigned": 8}},
		{name: "short alias", args: []string{"--tensor-parallel-size", "8", "-dp", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"required": 16}},
		{name: "inline value", args: []string{"--tensor-parallel-size=4", "--data-parallel-size=2"}, assigned: 8},
		{name: "replicas across nodes", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size", "2", "--data-parallel-size-local", "1"}, assigned: 8, nodes: 2},
		{name: "per-node local share is not the degree", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size-local", "2"}, assigned: 8},
		{name: "symbolic degree skips the equation", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size", "${DP}"}, assigned: 8},
		{name: "symbolic TP skips the equation", args: []string{"--tensor-parallel-size", "${TP}", "--data-parallel-size", "2"}, assigned: 8},
		{name: "symbolic PP skips the equation", args: []string{"--tensor-parallel-size", "8", "--pipeline-parallel-size", "${PP}", "--data-parallel-size", "2"}, assigned: 8},
		{name: "unstated TP and degree are not judged", args: []string{"--pipeline-parallel-size", "4"}, assigned: 8},
		{name: "unstated TP with degree one is not judged", args: []string{"--data-parallel-size", "1"}, assigned: 8},
		{name: "unstated TP replicates the engine default of one", args: []string{"--data-parallel-size", "2"}, assigned: 2},
		{name: "unstated TP replicas must still fill the world", args: []string{"--data-parallel-size", "2"}, assigned: 4,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"tp": 1, "required": 2, "assigned": 4}},
		{name: "non-positive degree", args: []string{"--tensor-parallel-size", "8", "--data-parallel-size", "0"}, assigned: 8,
			wantErr: "--data-parallel-size requires a positive integer"},
	})
}

// SGLang without DP attention starts dp replicas of tp × pp GPUs, the same
// arithmetic as vLLM; with it the attention replicas share the tensor-parallel
// GPUs, so the world stays tp × pp and dp has to divide tp.
func TestSGLangDataParallelFollowsDPAttention(t *testing.T) {
	runDataParallelCases(t, sglangEngine{}, []dataParallelCase{
		// Without --enable-dp-attention.
		{name: "unset degree keeps tp × pp", args: []string{"--tp-size", "8"}, assigned: 8},
		{name: "degree one keeps the tp × pp error", args: []string{"--tp-size", "4", "--dp-size", "1"}, assigned: 8,
			wantErr: "tp-size 4 × pp-size 1 requires 4 GPUs, but placement assigned 8"},
		{name: "injected TP covering the world leaves no GPU for a second replica", args: []string{"--tp-size", "8", "--dp-size", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"key": "data_parallel_size", "flag": "--dp-size", "dp": 2, "tp": 8, "pp": 1, "required": 16, "assigned": 8}},
		{name: "tp × dp equal to the world", args: []string{"--tp-size", "4", "--dp-size", "2"}, assigned: 8},
		{name: "tp × pp × dp equal to the world", args: []string{"--tp-size", "2", "--pp-size", "2", "--dp-size", "2"}, assigned: 8},
		{name: "tp × pp × dp below the world", args: []string{"--tp-size", "2", "--pp-size", "2", "--dp-size", "2"}, assigned: 16,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"required": 8, "assigned": 16}},
		{name: "short alias", args: []string{"--tp", "8", "--dp", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"required": 16}},
		{name: "long alias", args: []string{"--tp-size", "8", "--data-parallel-size", "2"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount},
		{name: "switch stated false", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention=false"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount},
		{name: "switch turned off by a later negation", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention", "--no-enable-dp-attention"}, assigned: 8,
			wantCode: ErrorDataParallelGPUCount},
		{name: "symbolic degree skips the equation", args: []string{"--tp-size", "4", "--dp-size", "${DP}"}, assigned: 8},
		{name: "unstated TP replicates the engine default of one", args: []string{"--dp-size", "2"}, assigned: 2},
		{name: "unstated TP replicas must still fill the world", args: []string{"--dp-size", "2"}, assigned: 4,
			wantCode: ErrorDataParallelGPUCount, wantParams: map[string]any{"tp": 1, "required": 2}},

		// With --enable-dp-attention.
		{name: "official recipe tp 8 dp 8", args: []string{"--tp", "8", "--dp", "8", "--enable-dp-attention"}, assigned: 8},
		{name: "degree dividing tp", args: []string{"--tp-size", "8", "--dp-size", "4", "--enable-dp-attention"}, assigned: 8},
		{name: "degree one", args: []string{"--tp-size", "8", "--dp-size", "1", "--enable-dp-attention"}, assigned: 8},
		{name: "switch stated true", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention=true"}, assigned: 8},
		{name: "switch stated as 1", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention=1"}, assigned: 8},
		{name: "switch turned back on", args: []string{"--no-enable-dp-attention", "--tp-size", "8", "--dp-size", "2", "--enable-dp-attention"}, assigned: 8},
		{name: "degree not dividing tp", args: []string{"--tp-size", "8", "--dp-size", "3", "--enable-dp-attention"}, assigned: 8,
			wantCode: ErrorDataParallelAttentionDivisor, wantParams: map[string]any{"key": "data_parallel_size", "flag": "--dp-size", "dp": 3, "tp": 8}},
		{name: "degree above tp", args: []string{"--tp-size", "4", "--dp-size", "8", "--enable-dp-attention"}, assigned: 4,
			wantCode: ErrorDataParallelAttentionDivisor, wantParams: map[string]any{"dp": 8, "tp": 4}},
		{name: "tp must still equal the world", args: []string{"--tp-size", "4", "--dp-size", "2", "--enable-dp-attention"}, assigned: 8,
			wantErr: "tp-size 4 × pp-size 1 requires 4 GPUs, but placement assigned 8"},
		{name: "symbolic degree still holds tp to the world", args: []string{"--tp-size", "4", "--dp-size", "${DP}", "--enable-dp-attention"}, assigned: 8,
			wantErr: "tp-size 4 × pp-size 1 requires 4 GPUs, but placement assigned 8"},
		{name: "symbolic switch skips the equation", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention=${DPA}"}, assigned: 8},
		{name: "symbolic TP skips the equation", args: []string{"--tp-size", "${TP}", "--dp-size", "3", "--enable-dp-attention"}, assigned: 8},
		{name: "unstated TP cannot host attention replicas", args: []string{"--dp-size", "2", "--enable-dp-attention"}, assigned: 1,
			wantCode: ErrorDataParallelAttentionDivisor, wantParams: map[string]any{"tp": 1}},
		{name: "switch with a non-boolean value", args: []string{"--tp-size", "8", "--dp-size", "2", "--enable-dp-attention=maybe"}, assigned: 8,
			wantErr: "--enable-dp-attention takes no value or a boolean"},

		// --num-gpus is the authoritative total of a diffusion command.
		{name: "num-gpus ignores the degree", args: []string{"--num-gpus", "8", "--tp-size", "8", "--dp-size", "2"}, assigned: 8},
	})
}

// The quick form's data_parallel_size reaches each engine through its own
// dictionary flag beside the TP placement injected; with nothing else stated
// the replicas ask for more GPUs than placement assigned, and ResolveLaunch -
// the path both the preview and the controller take - refuses.
func TestResolveLaunchRefusesQuickDataParallelBeyondTheWorld(t *testing.T) {
	for _, engine := range []Engine{vLLMEngine{}, sglangEngine{}} {
		t.Run(engine.Name(), func(t *testing.T) {
			params := Params{ModelPath: "/model", ServedName: "qwen", APIPort: 30000, AssignedGPUs: 4, TensorParallel: 4, Quick: LaunchConfig{DP: 2}}
			_, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: params})
			var productErr *messages.ProductError
			if !errors.As(err, &productErr) || productErr.Code() != ErrorDataParallelGPUCount {
				t.Fatalf("ResolveLaunch() = %v, want %s", err, ErrorDataParallelGPUCount)
			}
			if fmt.Sprint(productErr.Params()["required"]) != "8" || fmt.Sprint(productErr.Params()["assigned"]) != "4" {
				t.Fatalf("params = %v, want required 8 and assigned 4", productErr.Params())
			}

			params.Quick.DP = 1
			if _, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Params: params}); err != nil {
				t.Fatalf("ResolveLaunch() with degree one = %v, want accepted", err)
			}
		})
	}
}
