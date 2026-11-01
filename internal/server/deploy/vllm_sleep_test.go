package deploy

import (
	"encoding/json"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestResolveVLLMAscendSleepAdditionalConfig(t *testing.T) {
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
			input := vllmSleepLaunchInput("ascend")
			input.Params.TemplateArgs, input.Params.ExtraArgs = test.templateArgs, test.extraArgs
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
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
	product := requireLaunchProductError(t, err, ErrorAscendSleepWeightNZConflict)
	if want := map[string]any{"flag": "--additional-config", "value": "2"}; !reflect.DeepEqual(product.Params(), want) {
		t.Fatalf("params = %#v, want %#v", product.Params(), want)
	}
}

func TestResolveVLLMAscendSleepAcceptsZeroUserWeightNZ(t *testing.T) {
	input := vllmSleepLaunchInput("ascend")
	input.Params.ExtraArgs = []string{"--additional-config", `{"weight_nz_mode":0.0}`}
	if _, err := ResolveLaunch(input); err != nil {
		t.Fatal(err)
	}
}

func TestResolveVLLMAscendSleepRejectsNonObjectTemplateConfig(t *testing.T) {
	input := vllmSleepLaunchInput("ascend")
	input.Params.TemplateArgs = []string{"--additional-config", `[1]`}
	_, err := ResolveLaunch(input)
	product := requireLaunchProductError(t, err, ErrorAscendSleepAdditionalConfigInvalid)
	if want := map[string]any{"flag": "--additional-config", "value": "[1]"}; !reflect.DeepEqual(product.Params(), want) {
		t.Fatalf("params = %#v, want %#v", product.Params(), want)
	}
}

func TestResolveVLLMSleepLeavesOtherVendorsAndAwakeAscendUntouched(t *testing.T) {
	for name, input := range map[string]ResolveLaunchInput{
		"nvidia sleepable": vllmSleepLaunchInput("nvidia"),
		"ascend awake": func() ResolveLaunchInput {
			input := vllmSleepLaunchInput("ascend")
			input.Params.Sleepable = false
			return input
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			input.Params.TemplateArgs = []string{"--additional-config", `{"weight_nz_mode":2}`}
			input.Params.TemplateEnv = map[string]string{"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:True"}
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if got := allFlagValues(launch.Argv, "--additional-config"); !reflect.DeepEqual(got, []string{`{"weight_nz_mode":2}`}) {
				t.Fatalf("additional-config = %#v", got)
			}
			if got := launch.Env["PYTORCH_NPU_ALLOC_CONF"]; got != "expandable_segments:True" {
				t.Fatalf("PYTORCH_NPU_ALLOC_CONF = %q", got)
			}
		})
	}
}

func TestResolveVLLMAscendSleepChecksEffectiveAllocatorConfig(t *testing.T) {
	for name, test := range map[string]struct {
		overrides ContainerOverrides
		want      string
		wantError bool
	}{
		// The template layer is the platform's: its setting is turned off and
		// the other allocator settings are kept.
		"template expandable segments": {want: "max_split_size_mb:256,expandable_segments:False"},
		"user disables template setting": {
			overrides: ContainerOverrides{Env: map[string]string{"PYTORCH_NPU_ALLOC_CONF": "expandable_segments:False"}},
			want:      "expandable_segments:False",
		},
		"user unsets template setting": {
			overrides: ContainerOverrides{EnvUnset: []string{"PYTORCH_NPU_ALLOC_CONF"}},
		},
		"user enables expandable segments": {
			overrides: ContainerOverrides{Env: map[string]string{"PYTORCH_NPU_ALLOC_CONF": " Expandable_Segments : true "}}, wantError: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := vllmSleepLaunchInput("ascend")
			templateEnv := map[string]string{"PYTORCH_NPU_ALLOC_CONF": "max_split_size_mb:256,expandable_segments:True"}
			input.Params.TemplateEnv = templateEnv
			input.ContainerOverrides = test.overrides
			launch, err := ResolveLaunch(input)
			if templateEnv["PYTORCH_NPU_ALLOC_CONF"] != "max_split_size_mb:256,expandable_segments:True" {
				t.Fatalf("caller's template env was mutated: %#v", templateEnv)
			}
			if !test.wantError {
				if err != nil {
					t.Fatal(err)
				}
				if got := launch.Env["PYTORCH_NPU_ALLOC_CONF"]; got != test.want {
					t.Fatalf("PYTORCH_NPU_ALLOC_CONF = %q, want %q", got, test.want)
				}
				return
			}
			product := requireLaunchProductError(t, err, ErrorAscendSleepExpandableSegments)
			if want := map[string]any{"variable": "PYTORCH_NPU_ALLOC_CONF", "value": " Expandable_Segments : true "}; !reflect.DeepEqual(product.Params(), want) {
				t.Fatalf("params = %#v, want %#v", product.Params(), want)
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
