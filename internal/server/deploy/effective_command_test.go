package deploy

import (
	"reflect"
	"testing"
)

// The template entrypoint is the starting point; the deployment's override
// replaces it, and either keeps the engine dialect interpreted.
func TestResolveEffectiveCommandEntrypointPrecedence(t *testing.T) {
	template := []string{"template-entry"}

	fromTemplate, err := ResolveEffectiveCommand(ContainerOverrides{}, template)
	if err != nil {
		t.Fatal(err)
	}
	if fromTemplate.Mode != CommandEngine || !reflect.DeepEqual(fromTemplate.Entrypoint, template) || !fromTemplate.InterpretEngineDialect || !fromTemplate.templateEntrypoint {
		t.Fatalf("template command = %+v", fromTemplate)
	}

	overridden, err := ResolveEffectiveCommand(ContainerOverrides{EntrypointDisplay: `["user-entry"]`}, template)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(overridden.Entrypoint, []string{"user-entry"}) || !overridden.InterpretEngineDialect || overridden.templateEntrypoint {
		t.Fatalf("overridden command = %+v, want user entrypoint with dialect interpretation", overridden)
	}
}

func TestResolveEffectiveCommandManualDoesNotInterpretEngineDialect(t *testing.T) {
	command, err := ResolveEffectiveCommand(
		ContainerOverrides{
			CommandMode:       string(CommandManual),
			EntrypointDisplay: `["sglang","serve"]`,
			CmdDisplay:        `--model-type diffusion --enable-metrics`,
		},
		[]string{"python", "-m", "sglang.launch_server"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if command.Mode != CommandManual || command.InterpretEngineDialect {
		t.Fatalf("manual command = %+v, want uninterpreted manual mode", command)
	}
	if !reflect.DeepEqual(command.Entrypoint, []string{"sglang", "serve"}) {
		t.Fatalf("manual entrypoint = %v, want request override", command.Entrypoint)
	}
}

// image-entry with no stated entrypoint leaves it empty, so the container
// runtime uses the image's own ENTRYPOINT.
func TestResolveEffectiveCommandImageEntryLeavesTheImageEntrypoint(t *testing.T) {
	command, err := ResolveEffectiveCommand(ContainerOverrides{CommandMode: string(CommandImageEntry)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if command.Mode != CommandImageEntry || !command.InterpretEngineDialect || len(command.Entrypoint) != 0 {
		t.Fatalf("image-entry command = %+v", command)
	}
}

func TestResolveEffectiveCommandRefusesInvalidOverrides(t *testing.T) {
	for _, overrides := range []ContainerOverrides{
		{CommandMode: "shell"},
		{EntrypointDisplay: `["unterminated`},
		{DockerArgs: []string{"--privileged"}},
	} {
		if _, err := ResolveEffectiveCommand(overrides, nil); err == nil {
			t.Fatalf("%+v accepted", overrides)
		}
	}
}

func TestResolveLaunchDeploymentEntrypointBeatsTheTemplate(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine:             sglangEngine{},
		Params:             Params{ModelPath: "/model", TensorParallel: 1},
		ContainerOverrides: ContainerOverrides{EntrypointDisplay: `["python","-m","sglang.launch_server"]`},
		Entrypoint:         []string{"sglang", "serve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Entrypoint[:3], []string{"python", "-m", "sglang.launch_server"}) {
		t.Fatalf("entrypoint = %v, want the request override", launch.Entrypoint)
	}
}

func TestResolveLaunchExplicitCommandOwnsArgv(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: launchTestEngine{launch: ResolvedLaunch{
			Entrypoint: []string{"vllm", "serve", "/model"},
			Argv:       []string{"--port", "30000"},
		}},
		ContainerOverrides: ContainerOverrides{
			EntrypointDisplay: `["custom-engine"]`,
			CmdDisplay:        `["serve","/model"]`,
			ModelMountPath:    "/override-model",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Entrypoint, []string{"custom-engine"}) {
		t.Fatalf("entrypoint = %v, want explicit entrypoint", launch.Entrypoint)
	}
	if !reflect.DeepEqual(launch.Argv, []string{"serve", "/override-model"}) {
		t.Fatalf("argv = %v, want complete explicit command", launch.Argv)
	}
}
