package deploy

import (
	"strings"
	"testing"
)

// TestValidateTopologyMeasuresAssignedGPUs pins the meaning of the two counts:
// placement writes AssignedGPUs, and topology validation measures the effective
// argv against AssignedGPUs × NNodes. TensorParallel is the degree the engine
// would inject, so a template that splits the assigned world across TP and PP
// must validate even when TensorParallel disagrees with the template's TP.
func TestValidateTopologyMeasuresAssignedGPUs(t *testing.T) {
	for _, test := range []struct {
		name    string
		engine  Engine
		params  Params
		args    []string
		wantErr string
	}{
		{
			name:   "vLLM splits the assigned world across TP and PP",
			engine: vLLMEngine{},
			params: Params{AssignedGPUs: 4, TensorParallel: 1},
			args:   []string{"--tensor-parallel-size", "1", "--pipeline-parallel-size", "4"},
		},
		{
			name:    "vLLM rejects a template whose TP × PP exceeds the assigned GPUs",
			engine:  vLLMEngine{},
			params:  Params{AssignedGPUs: 4, TensorParallel: 4},
			args:    []string{"--tensor-parallel-size", "4", "--pipeline-parallel-size", "4"},
			wantErr: "requires 16 GPUs, but placement assigned 4",
		},
		{
			name:   "vLLM falls back to the stated parallelism when AssignedGPUs is unset",
			engine: vLLMEngine{},
			params: Params{TensorParallel: 2},
			args:   []string{"--tensor-parallel-size", "2"},
		},
		{
			name:    "vLLM fallback world is the stated parallelism, not one",
			engine:  vLLMEngine{},
			params:  Params{TensorParallel: 2},
			args:    []string{"--tensor-parallel-size", "1"},
			wantErr: "requires 1 GPUs, but placement assigned 2",
		},
		{
			name:   "SGLang tp-size covers the assigned GPUs of every node",
			engine: sglangEngine{},
			params: Params{AssignedGPUs: 4, TensorParallel: 1, NNodes: 2},
			args:   []string{"--tp-size", "8"},
		},
		{
			name:    "SGLang rejects tp-size derived from TensorParallel",
			engine:  sglangEngine{},
			params:  Params{AssignedGPUs: 4, TensorParallel: 1, NNodes: 2},
			args:    []string{"--tp-size", "2"},
			wantErr: "tp-size 2 × pp-size 1 requires 2 GPUs, but placement assigned 8",
		},
		{
			name:   "SGLang falls back to the stated parallelism when AssignedGPUs is unset",
			engine: sglangEngine{},
			params: Params{TensorParallel: 2},
			args:   []string{"--tp-size", "2"},
		},
		{
			name:    "SGLang fallback world is the stated parallelism, not one",
			engine:  sglangEngine{},
			params:  Params{TensorParallel: 2},
			args:    []string{"--tp-size", "1"},
			wantErr: "tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := test.params
			params.ModelPath = "/model"
			params.TemplateArgs = test.args
			_, err := ResolveLaunch(ResolveLaunchInput{Engine: test.engine, Params: params})
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ResolveLaunch() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ResolveLaunch() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

// TestAssignedGPUsDoesNotChangeInjectedTopology keeps the split honest in the
// other direction: AssignedGPUs is read by validation only, so the degree each
// engine injects still comes from TensorParallel alone.
func TestAssignedGPUsDoesNotChangeInjectedTopology(t *testing.T) {
	params := Params{ModelPath: "/model", ServedName: "qwen", APIPort: 30000, AssignedGPUs: 8, TensorParallel: 2}
	vllm := launchArguments(vLLMEngine{}.Build(params))
	if !argvHas(vllm, "--tensor-parallel-size", "2") {
		t.Errorf("vLLM command = %v, want --tensor-parallel-size 2", vllm)
	}
	sglang := launchArguments(sglangEngine{}.Build(params))
	if !argvHas(sglang, "--tp-size", "2") {
		t.Errorf("SGLang command = %v, want --tp-size 2", sglang)
	}
}
