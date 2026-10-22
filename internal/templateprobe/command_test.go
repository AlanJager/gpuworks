package templateprobe

import (
	"reflect"
	"testing"
)

func TestBuildL2CommandOwnsDummyAndParallelFlags(t *testing.T) {
	tests := []struct {
		name  string
		input L2CommandInput
		want  []string
	}{
		{
			name: "vllm strips separate and inline aliases",
			input: L2CommandInput{Engine: "vllm", Model: "/model", Config: ModelConfig{NumHiddenLayers: 32}, BaseArgv: []string{
				"vllm", "serve", "/model", "--tensor-parallel-size=8", "--pipeline-parallel-size", "2", "--load-format", "auto", "--hf-overrides", `{"foo":1}`, "--dtype", "bfloat16",
			}},
			want: []string{"vllm", "serve", "/model", "--dtype", "bfloat16", "--load-format", "dummy", "--tensor-parallel-size", "1", "--pipeline-parallel-size", "1", "--hf-overrides", `{"num_hidden_layers":2}`},
		},
		{
			name:  "sglang defaults",
			input: L2CommandInput{Engine: "sglang", Model: "/model path", Config: ModelConfig{NumHiddenLayers: 20}},
			want:  []string{"python", "-m", "sglang.launch_server", "--model-path", "/model path", "--load-format", "dummy", "--tp-size", "1", "--pp-size", "1", "--json-model-override-args", `{"num_hidden_layers":2}`},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := BuildL2Command(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Argv, test.want) {
				t.Fatalf("argv = %#v, want %#v", got.Argv, test.want)
			}
		})
	}
}

func TestBuildL2CommandRefusesUnknownMoE(t *testing.T) {
	experts := 8
	got, err := BuildL2Command(L2CommandInput{Engine: "vllm", Model: "/model", Config: ModelConfig{NumHiddenLayers: 40, NumLocalExperts: &experts}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Truncation.Allowed || len(got.Argv) != 0 {
		t.Fatalf("unknown MoE command = %+v, want refusal without argv", got)
	}
}
