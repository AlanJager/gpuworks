package deploy

import (
	"slices"
	"testing"
)

// The per-request metrics switch defaults on only for an image known to run
// vLLM 0.25.0 or later and without --disable-log-stats, which vLLM refuses to
// combine with it; anywhere else the default stays off the command line
// instead of stopping the engine from starting, and the switch is left to the
// engine. A deployer who turns it on gets the flag regardless.
func TestVLLMRequestMetricsDefaultFollowsTheImage(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		version  string
		template []string
		extra    []string
		raw      map[string]any
		wantFlag bool
		// leftToEngine: the default is withheld, so the switch starts at
		// automatic whatever the request then states.
		leftToEngine bool
	}{
		{name: "current release", version: "0.30.0", wantFlag: true},
		{name: "first release with it", version: "0.25.0", wantFlag: true},
		{name: "vllm-ascend release candidate", version: "0.26.0rc1", wantFlag: true},
		{name: "older release", version: "0.23.0", leftToEngine: true},
		{name: "version nobody recorded", version: "", leftToEngine: true},
		{name: "template disables stats", version: "0.30.0", template: []string{"--disable-log-stats"}, leftToEngine: true},
		{name: "user disables stats", version: "0.30.0", extra: []string{"--max-num-seqs", "8", "--disable-log-stats"}, leftToEngine: true},
		{name: "turned on for an unknown release", version: "", raw: map[string]any{"enable_request_metrics": true}, wantFlag: true, leftToEngine: true},
		{name: "turned off on a current release", version: "0.30.0", raw: map[string]any{"enable_request_metrics": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Command: &command, Raw: tc.raw, TemplateArgs: tc.template, ExtraArgs: tc.extra, EngineVersion: tc.version})
			if err != nil {
				t.Fatal(err)
			}
			p := goldenParams(out.Quick)
			p.TemplateArgs, p.ExtraArgs = out.TemplateResidual, out.UserResidual
			built := mustEngine(t, EngineVLLM).Build(p)
			if got := commandHasFlag(built.Argv, "--enable-per-request-metrics"); got != tc.wantFlag {
				t.Fatalf("flag present = %v, want %v: %v", got, tc.wantFlag, built.Argv)
			}
			if got := slices.Contains(out.EngineDecidesParamKeys, "enable_request_metrics"); got != tc.leftToEngine {
				t.Fatalf("left to the engine = %v, want %v", got, tc.leftToEngine)
			}
		})
	}
}

// A stored deployment states nothing about the parameter, so rebuilding its
// launch on a current image adds no flag: running deployments keep the
// command they were started with.
func TestVLLMRequestMetricsLeavesStoredLaunchesAlone(t *testing.T) {
	stored, err := materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"max_model_len": 4096}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Quick.EnableRequestMetrics != nil {
		t.Fatalf("stored row gained enable_request_metrics = %v", *stored.Quick.EnableRequestMetrics)
	}
	built := mustEngine(t, EngineVLLM).Build(goldenParams(stored.Quick))
	if commandHasFlag(built.Argv, "--enable-per-request-metrics") {
		t.Fatalf("stored launch gained the flag: %v", built.Argv)
	}
	// A new request states it through the dictionary default.
	fresh, err := Materialize(MaterializeInput{Engine: EngineVLLM})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Quick.EnableRequestMetrics == nil || !*fresh.Quick.EnableRequestMetrics {
		t.Fatalf("new request: enable_request_metrics = %v, want the default true", fresh.Quick.EnableRequestMetrics)
	}
	// SGLang has no such flag and so no such parameter.
	if _, ok := engineParams(EngineSGLang)["enable_request_metrics"]; ok {
		t.Fatal("sglang publishes enable_request_metrics")
	}
}
