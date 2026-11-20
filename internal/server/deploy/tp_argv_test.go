package deploy

import "testing"

func TestTensorParallelFromArgv(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		argv   []string
		want   int
		ok     bool
	}{
		{"vllm separate value", EngineVLLM, []string{"serve", "model", "--tensor-parallel-size", "4"}, 4, true},
		{"vllm equals form", EngineVLLM, []string{"--tensor-parallel-size=8"}, 8, true},
		{"vllm short alias", EngineVLLM, []string{"--tp", "2"}, 2, true},
		{"sglang tp-size", EngineSGLang, []string{"--tp-size", "4"}, 4, true},
		{"callable states no scheme", EngineCallable, []string{"--tensor-parallel-size", "4"}, 0, false},
		{"unknown engine", "nope", []string{"--tensor-parallel-size", "4"}, 0, false},
		{"flag absent", EngineVLLM, []string{"--max-num-seqs", "4"}, 0, false},
		{"flag at the end without a value", EngineVLLM, []string{"--tensor-parallel-size"}, 0, false},
		{"non-numeric value", EngineVLLM, []string{"--tensor-parallel-size", "$TP_SIZE"}, 0, false},
		{"zero is not a degree", EngineVLLM, []string{"--tensor-parallel-size=0"}, 0, false},
		{"a longer flag sharing the prefix is not it", EngineVLLM, []string{"--tensor-parallel-size-extra", "4"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := TensorParallelFromArgv(tc.engine, tc.argv)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
