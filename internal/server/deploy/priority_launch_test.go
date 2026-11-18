package deploy

import (
	"slices"
	"testing"
)

// Stored deployments created before command-aware eligibility may still have
// their switch on. The final public launch assembler protects those too.
func TestResolveLaunchPriorityUsesEffectiveCommand(t *testing.T) {
	for _, tc := range []struct {
		name, engine string
		entry, args  []string
		overrides    ContainerOverrides
		want         bool
	}{
		{name: "supported", engine: EngineSGLang, want: true},
		{name: "diffusion", engine: EngineSGLang, entry: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}},
		{name: "sglang override", engine: EngineSGLang, entry: []string{"sglang", "serve", "--model-type", "diffusion"}, overrides: ContainerOverrides{EntrypointDisplay: "python -m sglang.launch_server"}, want: true},
		{name: "vllm mismatched", engine: EngineSGLang, entry: []string{"vllm", "serve"}},
		{name: "omni", engine: EngineVLLM, entry: []string{"vllm", "serve"}, args: []string{"--omni"}},
		{name: "entrypoint policy conflict", engine: EngineSGLang, entry: []string{"python", "-m", "sglang.launch_server", "--schedule-policy", "lpm"}},
		{name: "user fixes entrypoint policy", engine: EngineSGLang, entry: []string{"python", "-m", "sglang.launch_server", "--schedule-policy", "lpm"}, args: []string{"--schedule-policy", "fcfs"}, want: true},
		{name: "explicit command owns args", engine: EngineVLLM, overrides: ContainerOverrides{CmdDisplay: `["/model"]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine, err := SelectEngine(tc.engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{Engine: engine, Image: "example/engine:test", Entrypoint: tc.entry, ContainerOverrides: tc.overrides, Params: Params{ModelPath: "/model", PriorityScheduling: true, ExtraArgs: tc.args}})
			if err != nil {
				t.Fatal(err)
			}
			command := append(launch.Entrypoint, launch.Argv...)
			got := slices.Contains(command, "--enable-priority-scheduling") || slices.Contains(command, "--scheduling-policy")
			if got != tc.want {
				t.Fatalf("priority=%v want=%v command=%v", got, tc.want, command)
			}
		})
	}
}
