package deploy

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestServedModelNameReadsTheLaunch(t *testing.T) {
	for _, c := range []struct {
		name   string
		launch proto.ResolvedLaunch
		want   string
	}{
		{"flag and value", proto.ResolvedLaunch{Entrypoint: []string{"vllm", "serve", "/model"}, Argv: []string{"--served-model-name", "Qwen/Qwen3-0.6B", "--port", "30000"}}, "Qwen/Qwen3-0.6B"},
		{"flag=value", proto.ResolvedLaunch{Argv: []string{"python3", "-m", "sglang.launch_server", "--served-model-name=q06"}}, "q06"},
		{"the last one wins", proto.ResolvedLaunch{Argv: []string{"--served-model-name", "a", "--served-model-name", "b"}}, "b"},
		{"inside a manual script", proto.ResolvedLaunch{Entrypoint: []string{"/bin/sh"}, Argv: []string{"-c", `set -a; vllm serve /model --served-model-name "my model"`}}, "my model"},
		{"left to the shell", proto.ResolvedLaunch{Argv: []string{"--served-model-name", "$NAME"}}, ""},
		{"none", proto.ResolvedLaunch{Argv: []string{"llama-server", "-m", "/model/x.gguf"}}, ""},
	} {
		if got := ServedModelName(c.launch); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
