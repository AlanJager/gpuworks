package deploy

import (
	"reflect"
	"slices"
	"testing"
)

func TestResolveLaunchImageEntryRetainsSGLangEngineArguments(t *testing.T) {
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: sglangEngine{},
		ContainerOverrides: ContainerOverrides{
			CommandMode:       string(CommandImageEntry),
			EntrypointDisplay: `["python","-m","sglang.launch_server"]`,
		},
		Params: Params{
			ModelPath: "/model", APIPort: 30000,
			AssignedGPUs: 1, TensorParallel: 1,
			Quick: LaunchConfig{EnableMetrics: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(launch.Entrypoint, []string{"python", "-m", "sglang.launch_server"}) {
		t.Fatalf("image entrypoint changed: %v", launch.Entrypoint)
	}
	if !argvHas(launch.Argv, "--model-path", "/model") || !argvHas(launch.Argv, "--port", "30000") || !slices.Contains(launch.Argv, "--enable-metrics") {
		t.Fatalf("image-entry lost engine arguments: %v", launch.Argv)
	}
}
