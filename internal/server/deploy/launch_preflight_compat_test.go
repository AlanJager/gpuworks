package deploy

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestAscendCompatManualShellPreflight(t *testing.T) {
	for name, test := range map[string]struct {
		overrideV2 string
		scriptV2   string
	}{
		"UI generated V2 manual":                {overrideV2: "1", scriptV2: "1"},
		"manual runner env edit is not trusted": {overrideV2: "1", scriptV2: "0"},
	} {
		t.Run(name, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CommandIncludesPreparation: true,
				CmdDisplay: "# environment\nVLLM_USE_V2_MODEL_RUNNER=" + test.scriptV2 + "\n\n# launch\nvllm serve /model --port 30000",
				Env:        map[string]string{"VLLM_USE_V2_MODEL_RUNNER": test.overrideV2},
			}
			_, err := ResolveLaunch(input)
			product := requireLaunchProductError(t, err, messages.ErrorAscendCompatManualUnsupported)
			if product.Params()["runtime"] != proto.RuntimePreferenceUdocker || product.Params()["image"] != ascendCompatTestImage {
				t.Fatalf("error params = %#v", product.Params())
			}
		})
	}
	t.Run("Docker manual needs no PRoot overlay", func(t *testing.T) {
		input := ascendCompatLaunchInput(proto.RuntimePreferenceDocker)
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = "vllm serve /model"
		launch, err := ResolveLaunch(input)
		if err != nil || launch.PreStartScript != "" {
			t.Fatalf("Docker manual = %+v, %v", launch, err)
		}
	})
}

func TestMergedPreStartScriptLimitIsCheckedBeforeTheAgent(t *testing.T) {
	input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
	input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
	input.PreStartScript = strings.Repeat("x", maxResolvedPreStartScriptBytes)
	_, err := ResolveLaunch(input)
	product := requireLaunchProductError(t, err, messages.ErrorPreStartScriptTooLarge)
	if product.Params()["max_bytes"] != maxResolvedPreStartScriptBytes {
		t.Fatalf("error params = %#v", product.Params())
	}
	if got, ok := product.Params()["bytes"].(int); !ok || got <= maxResolvedPreStartScriptBytes {
		t.Fatalf("merged script size = %#v", product.Params()["bytes"])
	}
}

func TestEngramManualShellPreflight(t *testing.T) {
	for _, runtime := range []string{proto.RuntimePreferenceDocker, proto.RuntimePreferenceUdocker} {
		t.Run(runtime, func(t *testing.T) {
			input := engramLaunchInput(runtime)
			input.ContainerOverrides = ContainerOverrides{
				CommandMode: string(CommandManual), CommandIncludesPreparation: true,
				CmdDisplay: `vllm serve /model --engram-config '{"cpu_offload":true,"dp_shared_memory":true}'`,
			}
			_, err := ResolveLaunch(input)
			product := requireLaunchProductError(t, err, messages.ErrorEngramManualUnsupported)
			if product.Params()["flag"] != "--engram-config" {
				t.Fatalf("error params = %#v", product.Params())
			}
		})
	}
	t.Run("ordinary manual udocker is unchanged", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.TemplateArgs = nil
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = "vllm serve /model"
		launch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatal(err)
		}
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
			t.Fatalf("ordinary manual resources changed: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
		}
	})
}

func TestEngramLastValidConfigControlsResourcesAndPreflight(t *testing.T) {
	disabled := `{"cpu_offload":true,"dp_shared_memory":false}`
	for name, test := range map[string]struct {
		first, second string
		wantShared    bool
	}{
		"true then false does not reject": {first: engramTestConfig, second: disabled},
		"false then true rejects":         {first: disabled, second: engramTestConfig, wantShared: true},
	} {
		t.Run(name, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceUdocker)
			input.Params.TemplateArgs = []string{"--engram-config", test.first, "--engram-config=" + test.second}
			launch, err := ResolveLaunch(input)
			if test.wantShared {
				requireLaunchProductError(t, err, messages.ErrorEngramSharedMemoryRuntime)
				return
			}
			if err != nil {
				t.Fatalf("last disabled config was rejected: %v", err)
			}
			if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
				t.Fatalf("last disabled config kept Engram resources: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
			}
		})
	}
}

func TestOpaqueShellJSONPreflight(t *testing.T) {
	for _, mode := range []string{string(CommandManual), string(CommandEngine)} {
		t.Run("compat "+mode, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides.CommandMode = mode
			input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
			if mode == string(CommandManual) {
				input.ContainerOverrides.CmdDisplay = `["/bin/sh","-c","VLLM_USE_V2_MODEL_RUNNER=0 vllm serve /model"]`
			} else {
				input.Entrypoint = []string{"/bin/sh"}
				input.ContainerOverrides.CmdDisplay = `["-c","VLLM_USE_V2_MODEL_RUNNER=0 vllm serve /model"]`
			}
			_, err := ResolveLaunch(input)
			requireLaunchProductError(t, err, messages.ErrorAscendCompatManualUnsupported)
		})
		t.Run("engram "+mode, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceUdocker)
			input.ContainerOverrides.CommandMode = mode
			if mode == string(CommandManual) {
				input.ContainerOverrides.CmdDisplay = `["/bin/sh","-c","vllm serve /model --engram-config '{\"cpu_offload\":true,\"dp_shared_memory\":true}'"]`
			} else {
				input.Entrypoint = []string{"/bin/sh"}
				input.ContainerOverrides.CmdDisplay = `["-c","vllm serve /model --engram-config '{\"cpu_offload\":true,\"dp_shared_memory\":true}'"]`
			}
			_, err := ResolveLaunch(input)
			requireLaunchProductError(t, err, messages.ErrorEngramManualUnsupported)
		})
	}
}
