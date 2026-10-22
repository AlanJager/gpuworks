package deploy

import (
	"reflect"
	"testing"
)

var runveilThreadCaps = map[string]string{
	"OMP_NUM_THREADS": "4", "MKL_NUM_THREADS": "4", "OPENBLAS_NUM_THREADS": "4",
	"NUMEXPR_NUM_THREADS": "4", "TORCH_NUM_THREADS": "4",
}

func resolveOnRuntime(t *testing.T, engineName, runtimeDriver string, templateEnv map[string]string, overrides ContainerOverrides) ResolvedLaunch {
	t.Helper()
	eng, err := SelectEngine(engineName)
	if err != nil {
		t.Fatal(err)
	}
	launch, err := ResolveLaunch(ResolveLaunchInput{
		Engine: eng,
		Params: Params{
			ModelPath: "/model", TensorParallel: 8, AssignedGPUs: 8, NNodes: 1,
			Quick: materializedConfig(t, engineName), TemplateEnv: templateEnv,
			RuntimeDriver: runtimeDriver,
		},
		ContainerOverrides: overrides,
	})
	if err != nil {
		t.Fatal(err)
	}
	return launch
}

func threadCapsIn(env map[string]string) map[string]string {
	got := map[string]string{}
	for key := range runveilThreadCaps {
		if value, ok := env[key]; ok {
			got[key] = value
		}
	}
	return got
}

func TestThreadCapsForSGLangAndVLLMOnRunveil(t *testing.T) {
	for _, tc := range []struct {
		engine, runtime string
		want            map[string]string
	}{
		{EngineSGLang, "udocker", runveilThreadCaps},
		{EngineSGLang, " UDocker ", runveilThreadCaps},
		{EngineSGLang, "docker", map[string]string{}},
		{EngineSGLang, "nerdctl", map[string]string{}},
		// A preview with no node chosen does not know the runtime.
		{EngineSGLang, "", map[string]string{}},
		{EngineVLLM, "udocker", runveilThreadCaps},
		{EngineVLLM, " UDocker ", runveilThreadCaps},
		{EngineVLLM, "docker", map[string]string{}},
		{EngineVLLM, "nerdctl", map[string]string{}},
		{EngineVLLM, "", map[string]string{}},
	} {
		t.Run(tc.engine+"/"+tc.runtime, func(t *testing.T) {
			launch := resolveOnRuntime(t, tc.engine, tc.runtime, nil, ContainerOverrides{})
			if got := threadCapsIn(launch.Env); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("launch env thread caps = %v, want %v", got, tc.want)
			}
			// The preview shows them as the platform's own layer.
			if got := threadCapsIn(launch.EnvLayers.System); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("system layer thread caps = %v, want %v", got, tc.want)
			}
		})
	}
	if _, known := runveilEnv(EngineSGLang, Params{RuntimeDriver: "udocker"})["TOKENIZERS_PARALLELISM"]; known {
		t.Fatal("TOKENIZERS_PARALLELISM was never measured on a node and must not be a default")
	}
}

// The caps are only the lowest layer: a template or the deployment states its
// own value, and a deployment can remove any of them, which is how the deploy
// form deletes one.
func TestThreadCapsYieldToTemplateAndDeployment(t *testing.T) {
	for _, engineName := range []string{EngineSGLang, EngineVLLM} {
		t.Run(engineName, func(t *testing.T) {
			launch := resolveOnRuntime(t, engineName, "udocker",
				map[string]string{"OMP_NUM_THREADS": "8"},
				ContainerOverrides{
					EnvUnset: []string{"MKL_NUM_THREADS", "TORCH_NUM_THREADS"},
					Env:      map[string]string{"OPENBLAS_NUM_THREADS": "1"},
				})
			want := map[string]string{"OMP_NUM_THREADS": "8", "OPENBLAS_NUM_THREADS": "1", "NUMEXPR_NUM_THREADS": "4"}
			if got := threadCapsIn(launch.Env); !reflect.DeepEqual(got, want) {
				t.Fatalf("launch env thread caps = %v, want %v", got, want)
			}
		})
	}
}
