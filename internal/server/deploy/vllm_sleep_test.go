package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestBuildVLLMAscendSleepAdditionalConfig(t *testing.T) {
	for name, test := range map[string]struct {
		templateArgs []string
		extraArgs    []string
		want         map[string]any
	}{
		"default": {
			want: map[string]any{"weight_nz_mode": float64(0)},
		},
		"template JSON keeps other keys and replaces its NZ default": {
			templateArgs: []string{"--additional-config", `{"weight_nz_mode":2,"template_key":"kept","nested":{"enabled":true}}`},
			want:         map[string]any{"weight_nz_mode": float64(0), "template_key": "kept", "nested": map[string]any{"enabled": true}},
		},
		"user JSON keeps other keys": {
			extraArgs: []string{`--additional-config={"user_key":"kept"}`},
			want:      map[string]any{"weight_nz_mode": float64(0), "user_key": "kept"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			launch := BuildVLLM(Params{
				ModelPath: "/model", APIPort: 30000, TensorParallel: 1,
				Sleepable: true, Vendor: "ascend", TemplateArgs: test.templateArgs, ExtraArgs: test.extraArgs,
			})
			values := allFlagValues(launch.Argv, "--additional-config")
			if len(values) != 1 {
				t.Fatalf("additional-config values = %#v, argv = %#v", values, launch.Argv)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(values[0]), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("additional-config = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestResolveVLLMAscendSleepRejectsNonzeroUserWeightNZ(t *testing.T) {
	input := vllmSleepLaunchInput("ascend")
	input.Params.ExtraArgs = []string{"--additional-config", `{"weight_nz_mode":2,"other":true}`}
	_, err := ResolveLaunch(input)
	product := requireLaunchProductError(t, err, messages.ErrorFieldInvalid)
	if !strings.Contains(product.Error(), "weight_nz_mode=0") || product.Params()["key"] != "weight_nz_mode" {
		t.Fatalf("error = %v, params = %#v", product, product.Params())
	}
}

func TestResolveVLLMAscendSleepChecksEffectiveAllocatorConfig(t *testing.T) {
	for name, test := range map[string]struct {
		overrides ContainerOverrides
		wantError bool
	}{
		"template expandable segments": {wantError: true},
		"user disables template setting": {
			overrides: ContainerOverrides{Env: map[string]string{"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:False"}},
		},
		"user enables expandable segments": {
			overrides: ContainerOverrides{Env: map[string]string{"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:True"}}, wantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := vllmSleepLaunchInput("ascend")
			input.Params.TemplateEnv = map[string]string{"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:True"}
			input.ContainerOverrides = test.overrides
			_, err := ResolveLaunch(input)
			if !test.wantError {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			product := requireLaunchProductError(t, err, messages.ErrorFieldInvalid)
			if !strings.Contains(product.Error(), "expandable_segments") || product.Params()["variable"] != "PYTORCH_NPU_ALLOC_CONF" {
				t.Fatalf("error = %v, params = %#v", product, product.Params())
			}
		})
	}
}

func TestResolveVLLMHygonSleepCUDARTPath(t *testing.T) {
	for name, test := range map[string]struct {
		runtime     string
		templateEnv map[string]string
		userEnv     map[string]string
		want        string
	}{
		"docker":   {runtime: proto.RuntimePreferenceDocker, want: vllmHygonCUDARTPath},
		"nerdctl":  {runtime: proto.RuntimePreferenceNerdctl, want: vllmHygonCUDARTPath},
		"template": {runtime: proto.RuntimePreferenceDocker, templateEnv: map[string]string{"VLLM_CUDART_SO_PATH": "/template/libcudart.so"}, want: "/template/libcudart.so"},
		"user":     {runtime: proto.RuntimePreferenceNerdctl, userEnv: map[string]string{"VLLM_CUDART_SO_PATH": "/user/libcudart.so"}, want: "/user/libcudart.so"},
	} {
		t.Run(name, func(t *testing.T) {
			input := vllmSleepLaunchInput("hygon")
			input.Params.RuntimeDriver = test.runtime
			input.Params.TemplateEnv = test.templateEnv
			input.ContainerOverrides.Env = test.userEnv
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if got := launch.Env["VLLM_CUDART_SO_PATH"]; got != test.want {
				t.Fatalf("VLLM_CUDART_SO_PATH = %q, want %q", got, test.want)
			}
			if got := launch.EnvLayers.System["VLLM_CUDART_SO_PATH"]; got != vllmHygonCUDARTPath {
				t.Fatalf("system VLLM_CUDART_SO_PATH = %q, want %q", got, vllmHygonCUDARTPath)
			}
		})
	}
}

func vllmSleepLaunchInput(vendor string) ResolveLaunchInput {
	return ResolveLaunchInput{
		Engine: vLLMEngine{},
		Params: Params{
			ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1,
			Sleepable: true, Vendor: vendor, PositionalModel: true,
		},
	}
}
