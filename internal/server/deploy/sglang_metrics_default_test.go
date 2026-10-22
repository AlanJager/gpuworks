package deploy

import "testing"

// SGLang's Prometheus switch defaults on for a new deployment whose server
// takes it: the LLM server always, the diffusion server from v0.5.21. Where
// `sglang serve` decides the model type by itself it defaults on only when
// both servers of the release take it - though a deployer who asks for it
// there gets it. A stored deployment keeps the command it was started with.
func TestSGLangMetricsDefault(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entrypoint []string
		args       []string
		version    string
		want       bool
	}{
		{name: "adapter default launch", want: true},
		{name: "launch_server", entrypoint: []string{"python3", "-m", "sglang.launch_server"}, want: true},
		{name: "serve as LLM", entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "llm"}, want: true},
		{name: "serve detecting the model type", entrypoint: []string{"sglang", "serve"}, args: []string{"--num-gpus", "1"}},
		{name: "serve as diffusion", entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}},
		{name: "diffusion before v0.5.21", entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, version: "0.5.18"},
		{name: "diffusion v0.5.21", entrypoint: []string{"sglang", "serve"}, args: []string{"--model-type", "diffusion"}, version: "0.5.21", want: true},
		{name: "detected before v0.5.21", entrypoint: []string{"sglang", "serve"}, version: "0.5.18"},
		{name: "detected v0.5.21", entrypoint: []string{"sglang", "serve"}, version: "0.5.21", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := MaterializeInput{Engine: EngineSGLang, TemplateArgs: tc.args, EngineVersion: tc.version}
			if tc.entrypoint != nil {
				command, err := ResolveEffectiveCommand(ContainerOverrides{}, tc.entrypoint)
				if err != nil {
					t.Fatal(err)
				}
				input.Command = &command
			}
			got, err := Materialize(input)
			if err != nil {
				t.Fatal(err)
			}
			if got.Quick.EnableMetrics != tc.want {
				t.Fatalf("enable_metrics = %v, want %v", got.Quick.EnableMetrics, tc.want)
			}
		})
	}

	// Asked for explicitly on `sglang serve` without a model type, the switch
	// is the deployer's choice and goes out.
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, []string{"sglang", "serve"})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := Materialize(MaterializeInput{Engine: EngineSGLang, Command: &command, Raw: map[string]any{"enable_metrics": true}})
	if err != nil {
		t.Fatalf("explicit enable_metrics on sglang serve: %v", err)
	}
	if !asked.Quick.EnableMetrics {
		t.Fatal("explicit enable_metrics was dropped")
	}

	stored, err := materialize(MaterializeInput{Engine: EngineSGLang, Raw: map[string]any{"max_model_len": 4096}}, true)
	if err != nil {
		t.Fatal(err)
	}
	built := mustEngine(t, EngineSGLang).Build(goldenParams(stored.Quick))
	if stored.Quick.EnableMetrics || commandHasFlag(built.Entrypoint, "--enable-metrics") {
		t.Fatalf("stored launch gained --enable-metrics: %v", built.Entrypoint)
	}
}
