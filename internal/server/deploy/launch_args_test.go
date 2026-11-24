package deploy

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestLaunchEngineArgs(t *testing.T) {
	command := []string{"python3", "-m", "sglang.launch_server", "--mem-fraction-static", "0.90"}
	for _, tc := range []struct {
		name   string
		launch proto.ResolvedLaunch
		want   []string
	}{
		{
			name:   "platform assembled engine",
			launch: proto.ResolvedLaunch{CommandMode: string(CommandEngine), Entrypoint: command[:3], Argv: command[3:]},
			want:   command,
		},
		{
			name: "recognizable shell script",
			launch: proto.ResolvedLaunch{
				CommandMode: string(CommandManual), Entrypoint: []string{"/bin/sh"},
				Argv: []string{"-c", `# python3 -m sglang.launch_server --mem-fraction-static 0.88
echo --disable-cuda-graph
exec python3 -m sglang.launch_server --mem-fraction-static 0.90`},
			},
			want: command,
		},
		{
			name:   "explicit manual argv",
			launch: proto.ResolvedLaunch{CommandMode: string(CommandManual), Argv: command},
			want:   command,
		},
		{
			name:   "opaque wrapper",
			launch: proto.ResolvedLaunch{CommandMode: string(CommandManual), Argv: []string{"bash", "/opt/start.sh", "--mem-fraction-static", "0.90"}},
		},
		{
			name: "multiple possible engine invocations",
			launch: proto.ResolvedLaunch{
				CommandMode: string(CommandManual), Entrypoint: []string{"/bin/sh"},
				Argv: []string{"-c", `if test "$USE_HIGH" = 1; then
  exec python3 -m sglang.launch_server --mem-fraction-static 0.90
else
  exec python3 -m sglang.launch_server --mem-fraction-static 0.88
fi`},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := LaunchEngineArgs(EngineSGLang, tc.launch); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("LaunchEngineArgs = %#v, want %#v", got, tc.want)
			}
		})
	}
}

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
