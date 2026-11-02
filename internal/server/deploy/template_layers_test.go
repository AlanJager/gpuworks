package deploy

import (
	"reflect"
	"testing"
)

// TestTemplateArgumentPrecedenceMatrix measures the whole write path, because
// precedence is decided at materialization now and only passthrough layering is
// left to the build. Every case asserts the flag appears exactly once: one value
// reaching the engine is the property, not merely the right value winning.
func TestTemplateArgumentPrecedenceMatrix(t *testing.T) {
	tests := []struct {
		name         string
		raw          map[string]any
		templateArgs []string
		userArgs     []string
		flag         string
		want         string
	}{
		// --tensor-parallel-size is not a dictionary key, so it stays a
		// passthrough token and still suppresses the platform's own injection.
		{name: "template overrides engine injection", templateArgs: []string{"--tensor-parallel-size", "4"}, flag: "--tensor-parallel-size", want: "4"},
		{name: "form overrides template", raw: map[string]any{"max_model_len": 8192}, templateArgs: []string{"--max-model-len", "4096"}, flag: "--max-model-len", want: "8192"},
		{name: "extra args override template", templateArgs: []string{"--max-model-len", "4096"}, userArgs: []string{"--max-model-len", "16384"}, flag: "--max-model-len", want: "16384"},
		{name: "inline extra args override template", templateArgs: []string{"--max-model-len", "4096"}, userArgs: []string{"--max-model-len=32768"}, flag: "--max-model-len", want: "32768"},
		{name: "form overrides template utilization", raw: map[string]any{"gpu_memory_utilization": 0.8}, templateArgs: []string{"--gpu-memory-utilization", "0.95"}, flag: "--gpu-memory-utilization", want: "0.8"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			materialized, err := Materialize(MaterializeInput{
				Engine: EngineVLLM, Raw: test.raw, TemplateArgs: test.templateArgs, ExtraArgs: test.userArgs,
			})
			if err != nil {
				t.Fatal(err)
			}
			result := BuildVLLM(Params{
				ModelPath: "/model", TensorParallel: 1, Quick: materialized.Quick,
				TemplateArgs: materialized.TemplateResidual, ExtraArgs: materialized.UserResidual,
			})
			values := allFlagValues(result.Argv, test.flag)
			if len(values) != 1 || values[0] != test.want {
				t.Fatalf("argv=%#v %s values=%#v, want one %q", result.Argv, test.flag, values, test.want)
			}
		})
	}
}

func TestTemplateCacheReportFlagYieldsToTheConfiguredValue(t *testing.T) {
	for _, test := range []struct{ engine, flag string }{
		{EngineVLLM, "--enable-prompt-tokens-details"},
		{EngineSGLang, "--enable-cache-report"},
	} {
		t.Run(test.engine, func(t *testing.T) {
			count := func(raw map[string]any) int {
				materialized, err := Materialize(MaterializeInput{Engine: test.engine, Raw: raw, TemplateArgs: []string{test.flag}})
				if err != nil {
					t.Fatal(err)
				}
				engine, _ := SelectEngine(test.engine)
				launch := engine.Build(Params{ModelPath: "/model", TensorParallel: 1, Quick: materialized.Quick, TemplateArgs: materialized.TemplateResidual, ExtraArgs: materialized.UserResidual})
				n := 0
				for _, token := range append(append([]string(nil), launch.Entrypoint...), launch.Argv...) {
					if token == test.flag {
						n++
					}
				}
				return n
			}
			if n := count(map[string]any{"enable_cache_report": true}); n != 1 {
				t.Fatalf("field true + template flag: count=%d, want exactly one", n)
			}
			if n := count(map[string]any{"enable_cache_report": false}); n != 0 {
				t.Fatalf("field false overrides the template flag: count=%d, want none", n)
			}
			if n := count(nil); n != 1 {
				t.Fatalf("template flag alone: count=%d, want exactly one", n)
			}
		})
	}
}

func TestTemplateJSONAndSpaceArgumentsRemainTokens(t *testing.T) {
	jsonValue := `{"method":"eagle", "draft_model":"path with spaces"}`
	result := BuildVLLM(Params{
		ModelPath: "/model", TensorParallel: 1,
		TemplateArgs: []string{"--speculative-config", jsonValue, "--chat-template", "/model/chat templates/qwen.jinja"},
	})
	wantTail := []string{"--speculative-config", jsonValue, "--chat-template", "/model/chat templates/qwen.jinja"}
	if len(result.Argv) < len(wantTail) || !reflect.DeepEqual(result.Argv[len(result.Argv)-len(wantTail):], wantTail) {
		t.Fatalf("argv tail=%#v, want %#v", result.Argv, wantTail)
	}
}

func TestParseArgsTextPreservesQuotedAndJSONValues(t *testing.T) {
	input := `--speculative-config '{"method":"eagle", "draft_model":"path with spaces"}' --chat-template "/model/chat templates/qwen.jinja"`
	want := []string{"--speculative-config", `{"method":"eagle", "draft_model":"path with spaces"}`, "--chat-template", "/model/chat templates/qwen.jinja"}
	got, err := ParseArgsText(input)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args=%#v, want %#v", got, want)
	}
	jsonArgs, err := ParseArgsText(`["--flag","value with spaces","--json","{\"a\": 1}"]`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(jsonArgs, []string{"--flag", "value with spaces", "--json", `{"a": 1}`}) {
		t.Fatalf("JSON args=%#v", jsonArgs)
	}
}

func TestTemplateEnvironmentIsAnExplicitPriorityLayer(t *testing.T) {
	built := ResolvedLaunch{
		Env:       map[string]string{"SYSTEM_ONLY": "system", "ORDER": "system", "HF_HUB_OFFLINE": "1", "TRANSFORMERS_OFFLINE": "1"},
		EnvLayers: EnvLayers{Template: map[string]string{"TEMPLATE_ONLY": "template", "ORDER": "template", "HF_HUB_OFFLINE": "0", "TRANSFORMERS_OFFLINE": "0"}},
	}
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine:             launchTestEngine{launch: built},
		ContainerOverrides: ContainerOverrides{Env: map[string]string{"USER_ONLY": "user", "ORDER": "user"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if launch.Env["ORDER"] != "user" || launch.Env["SYSTEM_ONLY"] != "system" || launch.Env["TEMPLATE_ONLY"] != "template" || launch.Env["USER_ONLY"] != "user" || launch.Env["HF_HUB_OFFLINE"] != "0" || launch.Env["TRANSFORMERS_OFFLINE"] != "0" {
		t.Fatalf("effective env=%#v", launch.Env)
	}
	if launch.EnvLayers.System["ORDER"] != "system" || launch.EnvLayers.Template["ORDER"] != "template" || launch.EnvLayers.User["ORDER"] != "user" {
		t.Fatalf("env layers=%#v", launch.EnvLayers)
	}
}

func allFlagValues(args []string, flag string) []string {
	var values []string
	for index, token := range args {
		if token == flag && index+1 < len(args) {
			values = append(values, args[index+1])
		}
		if len(token) > len(flag)+1 && token[:len(flag)+1] == flag+"=" {
			values = append(values, token[len(flag)+1:])
		}
	}
	return values
}
