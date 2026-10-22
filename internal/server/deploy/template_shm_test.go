package deploy

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

// A template spec states the /dev/shm its engine needs (shm_bytes). It is a
// floor under what the launch derives, and it never overrides what the
// deployment itself states.
func TestResolveLaunchShmTakesTheTemplateFloor(t *testing.T) {
	const profileShm = int64(16) << 30
	hostIPC := []string{"--ipc=host"}
	for name, test := range map[string]struct {
		engine    string
		template  int64
		tierGiB   float64
		overrides ContainerOverrides
		want      int64
	}{
		"template above the engine default":          {engine: EngineVLLM, template: 400 << 30, want: 400 << 30},
		"template below the engine default":          {engine: EngineVLLM, template: 4 << 30, want: profileShm},
		"template equal to the engine default":       {engine: EngineVLLM, template: profileShm, want: profileShm},
		"template below default plus tier":           {engine: EngineVLLM, template: 30 << 30, tierGiB: 19, want: profileShm + 19<<30},
		"template above default plus tier":           {engine: EngineVLLM, template: 100 << 30, tierGiB: 19, want: 100 << 30},
		"no template keeps the derived size":         {engine: EngineVLLM, tierGiB: 19, want: profileShm + 19<<30},
		"sglang takes the template too":              {engine: EngineSGLang, template: 64 << 30, want: 64 << 30},
		"callable takes the template too":            {engine: EngineCallable, template: 64 << 30, want: 64 << 30},
		"explicit deployment size wins over larger":  {engine: EngineVLLM, template: 400 << 30, overrides: ContainerOverrides{ShmBytes: 64 << 30}, want: 64 << 30},
		"explicit deployment size wins over smaller": {engine: EngineVLLM, template: 4 << 30, overrides: ContainerOverrides{ShmBytes: 64 << 30}, want: 64 << 30},
		"host IPC states no size":                    {engine: EngineVLLM, template: 400 << 30, overrides: ContainerOverrides{DockerArgs: hostIPC}, want: 0},
		"saturated tier stays saturated":             {engine: EngineVLLM, template: 400 << 30, tierGiB: 1e30, want: 1<<63 - 1},
	} {
		t.Run(name, func(t *testing.T) {
			engine, err := SelectEngine(test.engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{
				Engine: engine, ContainerOverrides: test.overrides, TemplateShmBytes: test.template,
				Params: Params{
					ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, Quick: LaunchConfig{KVOffloadingSize: test.tierGiB},
					TemplateArgs: []string{"--port", "${API_PORT}"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if launch.ShmBytes != test.want {
				t.Fatalf("shm = %d, want %d", launch.ShmBytes, test.want)
			}
		})
	}
}

// The template's figure does not excuse an explicit size from the CPU tier
// check: the size the deployer typed is still refused when it cannot hold the
// tier, however large the template's own figure is.
func TestResolveLaunchTemplateShmDoesNotRescueAnExplicitSizeBelowTheTier(t *testing.T) {
	_, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, TemplateShmBytes: 400 << 30, ContainerOverrides: ContainerOverrides{ShmBytes: 8 << 30},
		Params: Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, Quick: LaunchConfig{KVOffloadingSize: 19}},
	})
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorShmBelowKVCPUTier {
		t.Fatalf("ResolveLaunch() = %v, want %s", err, messages.ErrorShmBelowKVCPUTier)
	}
}

// The snapshot a deployment keeps carries the template's figure, and one
// written before the field existed reads as stating none.
func TestCatalogSelectionCarriesTemplateShmBytes(t *testing.T) {
	selection, err := DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"deepinfer","spec_source":"upstream","shm_bytes":429496729600}`))
	if err != nil {
		t.Fatal(err)
	}
	if selection.ShmBytes != 429496729600 {
		t.Fatalf("shm_bytes = %d", selection.ShmBytes)
	}
	encoded, err := selection.Encode()
	if err != nil || !strings.Contains(string(encoded), `"shm_bytes":429496729600`) {
		t.Fatalf("encoded = %s, %v", encoded, err)
	}
	old, err := DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream"}`))
	if err != nil || old.ShmBytes != 0 {
		t.Fatalf("old snapshot = %+v, %v", old, err)
	}
	if encoded, err := old.Encode(); err != nil || strings.Contains(string(encoded), "shm_bytes") {
		t.Fatalf("old snapshot re-encoded as %s, %v", encoded, err)
	}
}
