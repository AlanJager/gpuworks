package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const ascendCompatTestImage = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"

func ascendCompatLaunchInput(runtime string) ResolveLaunchInput {
	return ResolveLaunchInput{
		Engine: vLLMEngine{}, Image: ascendCompatTestImage,
		Params: Params{
			ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1,
			TensorParallel: 1, Vendor: "ascend", PositionalModel: true,
			RuntimeDriver: runtime,
		},
	}
}

func TestResolveLaunchInjectsV030RC2A3Compatibility(t *testing.T) {
	launch, err := ResolveLaunch(ascendCompatLaunchInput(proto.RuntimePreferenceUdocker))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		ascendV030RC2A3CompatMarker,
		"GPUWorks: installed NPU zeros/ones CPU-copy fallback",
		"def _gpuworks_process_rendezvous(",
		"socket.AF_UNIX",
		"hashlib.sha256",
		"torch.npu.manual_seed(seed)",
	} {
		if !strings.Contains(launch.PreStartScript, want) {
			t.Errorf("A3 compatibility script lacks %q", want)
		}
	}
	for _, unwanted := range []string{"socket.AF_INET", "zlib.crc32", "gpuworks-model-runner-init.lock"} {
		if strings.Contains(launch.PreStartScript, unwanted) {
			t.Errorf("A3 compatibility script contains obsolete %q", unwanted)
		}
	}

	dockerLaunch, err := ResolveLaunch(ascendCompatLaunchInput(proto.RuntimePreferenceDocker))
	if err != nil {
		t.Fatal(err)
	}
	if dockerLaunch.PreStartScript != "" {
		t.Fatal("Docker must run the fixed image without the PRoot-specific compatibility script")
	}
}

func TestAscendCompatibilityUsesFinalLaunchEnvironment(t *testing.T) {
	for name, test := range map[string]struct {
		templateEnv map[string]string
		overrides   ContainerOverrides
		wantMarker  string
		wantEnv     string
	}{
		"user override selects V2": {
			overrides:  ContainerOverrides{Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}},
			wantMarker: ascendV030RC2A3V2CompatMarker, wantEnv: "1",
		},
		"user nonzero integer selects V2": {
			overrides:  ContainerOverrides{Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "2"}},
			wantMarker: ascendV030RC2A3V2CompatMarker, wantEnv: "2",
		},
		"user negative integer selects V2": {
			overrides:  ContainerOverrides{Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "-1"}},
			wantMarker: ascendV030RC2A3V2CompatMarker, wantEnv: "-1",
		},
		"user zero integer selects V1": {
			overrides:  ContainerOverrides{Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "00"}},
			wantMarker: ascendV030RC2A3CompatMarker, wantEnv: "00",
		},
		"env unset removes template V2": {
			templateEnv: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"},
			overrides:   ContainerOverrides{EnvUnset: []string{"VLLM_USE_V2_MODEL_RUNNER"}},
			wantMarker:  ascendV030RC2A3CompatMarker,
		},
		"user V1 overrides template V2": {
			templateEnv: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"},
			overrides:   ContainerOverrides{Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "0"}},
			wantMarker:  ascendV030RC2A3CompatMarker, wantEnv: "0",
		},
		"user env applies after unset": {
			templateEnv: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "0"},
			overrides: ContainerOverrides{
				EnvUnset: []string{"VLLM_USE_V2_MODEL_RUNNER"}, Env: map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"},
			},
			wantMarker: ascendV030RC2A3V2CompatMarker, wantEnv: "1",
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.Params.TemplateEnv = test.templateEnv
			input.ContainerOverrides = test.overrides
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(launch.PreStartScript, test.wantMarker) {
				t.Fatalf("compatibility script does not contain %q", test.wantMarker)
			}
			if got := launch.Env["VLLM_USE_V2_MODEL_RUNNER"]; got != test.wantEnv {
				t.Fatalf("effective V2 env = %q, want %q", got, test.wantEnv)
			}
		})
	}
}

func TestAscendRC2A3FeaturesDoNotOverrideRunnerEnvironment(t *testing.T) {
	for feature, args := range map[string][]string{
		"watermark": {"--watermark-config", `{"algorithm":"dual_key_gumbel","key":42}`},
		"HiSparse":  {"--attention-config", `{"hisparse_config":{}}`},
	} {
		for _, envValue := range []string{"", "0"} {
			t.Run(feature+"/env="+envValue, func(t *testing.T) {
				input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
				input.Params.TemplateArgs = args
				if envValue != "" {
					input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": envValue}
				}
				launch, err := ResolveLaunch(input)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(launch.PreStartScript, ascendV030RC2A3CompatMarker) || strings.Contains(launch.PreStartScript, ascendV030RC2A3V2CompatMarker) {
					t.Fatalf("%s with env %q selected the wrong compatibility script", feature, envValue)
				}
			})
		}
	}
}

func TestAscendCompatibilityIsIdempotentAndImageSpecific(t *testing.T) {
	for _, compat := range []ascendRuntimeCompat{ascendRuntimeCompatV1, ascendRuntimeCompatV2} {
		marker := ascendV030RC2A3CompatMarker
		if compat == ascendRuntimeCompatV2 {
			marker = ascendV030RC2A3V2CompatMarker
		}
		existing := marker + "echo existing\n"
		if got := appendAscendRuntimeCompatibilityScript(compat, existing); got != existing {
			t.Fatalf("already-present script changed: %q", got)
		}
	}
	for _, test := range []struct {
		engine, image, runtime string
	}{
		{EngineSGLang, ascendCompatTestImage, "udocker"},
		{EngineVLLM, "quay.io/ascend/vllm-ascend:v0.30.0rc1-a3", "udocker"},
		{EngineVLLM, "vllm/vllm-openai:v0.30.0", "udocker"},
		{EngineVLLM, ascendCompatTestImage, "docker"},
		{EngineVLLM, ascendCompatTestImage, "nerdctl"},
	} {
		if got := ascendRuntimeCompatFor(test.engine, test.image, test.runtime, nil); got != ascendRuntimeCompatNone {
			t.Errorf("%s/%s/%s selected compatibility %v", test.engine, test.image, test.runtime, got)
		}
	}
}

func TestAscendCompatibilityPreflightRefusesOnlyUnsupportedLaunches(t *testing.T) {
	multiNode := func(runtime string) ResolveLaunchInput {
		input := ascendCompatLaunchInput(runtime)
		input.Params.NNodes = 2
		input.Params.TensorParallel = 2
		input.Params.MasterAddr = "10.0.0.1"
		input.Params.MasterPort = 29500
		input.Params.NodeAddress = "10.0.0.2"
		return input
	}
	t.Run("V1 loopback across nodes warns and still resolves", func(t *testing.T) {
		launch, warnings := resolveWithWarnings(t, multiNode(proto.RuntimePreferenceUdocker))
		requireWarningCodes(t, warnings, messages.WarningAscendV1CompatMultiNode)
		params := warnings[0].Params
		if params["nnodes"] != 2 || params["image"] != ascendCompatTestImage || params["runtime"] != proto.RuntimePreferenceUdocker {
			t.Fatalf("warning params = %#v", params)
		}
		if !strings.Contains(launch.PreStartScript, ascendV030RC2A3CompatMarker) {
			t.Fatal("a warned V1 launch must still carry its compatibility script")
		}
	})
	t.Run("V2 can span nodes and compose script preparation", func(t *testing.T) {
		input := multiNode(proto.RuntimePreferenceUdocker)
		input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
		input.PreStartScript = "echo template"
		launch, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
		if !strings.Contains(launch.PreStartScript, ascendV030RC2A3V2CompatMarker) || !strings.HasSuffix(launch.PreStartScript, "echo template") {
			t.Fatalf("V2 preparation did not compose: %q", launch.PreStartScript)
		}
	})
	t.Run("V2 rejects argv preparation", func(t *testing.T) {
		input := multiNode(proto.RuntimePreferenceUdocker)
		input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
		input.PreStartArgv = []string{"prepare"}
		_, err := ResolveLaunch(input)
		product := requireLaunchProductError(t, err, messages.ErrorAscendCompatPreStartArgv)
		if product.Params()["field"] != "pre_start_argv" {
			t.Fatalf("error params = %#v", product.Params())
		}
	})
	t.Run("V2 JSON manual remains inspectable", func(t *testing.T) {
		input := multiNode(proto.RuntimePreferenceUdocker)
		input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = `["vllm","serve","/model"]`
		launch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(launch.PreStartScript, ascendV030RC2A3V2CompatMarker) {
			t.Fatalf("V2 JSON manual lost its compatibility script")
		}
		if len(launch.Entrypoint) != 1 || launch.Entrypoint[0] != "vllm" || len(launch.Argv) != 2 || launch.Argv[0] != "serve" || launch.Argv[1] != "/model" {
			t.Fatalf("manual JSON did not produce an executable pre-start launch: entrypoint=%v argv=%v", launch.Entrypoint, launch.Argv)
		}
	})
	t.Run("Docker needs no compatibility overlay", func(t *testing.T) {
		input := multiNode(proto.RuntimePreferenceDocker)
		input.PreStartScript = "echo template"
		launch, err := ResolveLaunch(input)
		if err != nil || launch.PreStartScript != "echo template" {
			t.Fatalf("Docker launch = %+v, %v", launch, err)
		}
	})
	t.Run("non-compat manual command is not rejected", func(t *testing.T) {
		input := multiNode(proto.RuntimePreferenceUdocker)
		input.Image = "quay.io/ascend/vllm-ascend:v0.30.0rc1-a3"
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = "vllm serve /model"
		if _, err := ResolveLaunch(input); err != nil {
			t.Fatalf("ordinary manual launch rejected: %v", err)
		}
	})
}

func TestAscendV1CompatibilityWarnsAboutDataParallel(t *testing.T) {
	withDP := func(dp int) ResolveLaunchInput {
		input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.AssignedGPUs = 4 * dp
		input.Params.TensorParallel = 4
		input.Params.Quick = LaunchConfig{DP: dp}
		return input
	}
	t.Run("V1 with DP above one warns", func(t *testing.T) {
		_, warnings := resolveWithWarnings(t, withDP(4))
		requireWarningCodes(t, warnings, messages.WarningAscendV1CompatDataParallel)
		params := warnings[0].Params
		if params["dp"] != 4 || params["image"] != ascendCompatTestImage || params["runtime"] != proto.RuntimePreferenceUdocker {
			t.Fatalf("warning params = %#v", params)
		}
	})
	t.Run("a DP written as an extra argument warns too", func(t *testing.T) {
		input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.AssignedGPUs, input.Params.TensorParallel = 4, 2
		input.Params.ExtraArgs = []string{"--data-parallel-size=2"}
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings, messages.WarningAscendV1CompatDataParallel)
	})
	t.Run("V1 with DP one is silent", func(t *testing.T) {
		_, warnings := resolveWithWarnings(t, withDP(1))
		requireWarningCodes(t, warnings)
	})
	t.Run("V2 with DP above one is silent", func(t *testing.T) {
		input := withDP(4)
		input.ContainerOverrides.Env = map[string]string{"VLLM_USE_V2_MODEL_RUNNER": "1"}
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
	})
	t.Run("Docker with DP above one is silent", func(t *testing.T) {
		input := withDP(4)
		input.Params.RuntimeDriver = proto.RuntimePreferenceDocker
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
	})
}

func TestAscendV1CompatibilityRejectsTemplatePreparation(t *testing.T) {
	for _, test := range []struct {
		field  string
		argv   []string
		script string
	}{
		{field: "pre_start_argv", argv: []string{"prepare"}},
		{field: "pre_start_script", script: "echo prepare"},
	} {
		t.Run(test.field, func(t *testing.T) {
			input := ascendCompatLaunchInput(proto.RuntimePreferenceUdocker)
			input.PreStartArgv = test.argv
			input.PreStartScript = test.script
			_, err := ResolveLaunch(input)
			product := requireLaunchProductError(t, err, messages.ErrorAscendV1CompatPreStart)
			if product.Params()["field"] != test.field {
				t.Fatalf("error params = %#v", product.Params())
			}
		})
	}
}

func TestAscendV1CompatibilityScriptPatchesKnownLayoutIdempotently(t *testing.T) {
	dir := t.TempDir()
	runner := filepath.Join(dir, "model_runner_v1.py")
	worker := filepath.Join(dir, "worker.py")
	torchUtils := filepath.Join(dir, "torch_utils.py")
	fixtures := map[string]string{
		runner: `class NPUModelRunner:
    def __init__(self, vllm_config, device):
        with _torch_cuda_wrapper():
            super().__init__(vllm_config, device)
`,
		worker: `import torch

class NPUWorker:
    def _init_worker_distributed_environment(self) -> None:
        init_distributed_environment(
            self.parallel_config.world_size, self.rank, self.distributed_init_method, self.local_rank, "hccl"
        )

    def _init_device(self):
        self._init_worker_distributed_environment()
        # Set random seed.
        set_random_seed(self.model_config.seed)
        # Initialize device properties used by triton kernels.
        init_device_properties_triton()

        return device

    def init_device(self):
        self.device = self._init_device()
        # Init ModelRunner here, so that we have access to self.device.
        if self.use_v2_model_runner:
            self.model_runner = NPUModelRunnerV2(self.vllm_config, self.device)

    def init_distributed(self):
        init_ascend_model_parallel(self.parallel_config, self.vllm_config.engram_config)
        ensure_ec_transfer_initialized(self.vllm_config)
`,
		torchUtils: `def set_random_seed(seed):
        current_platform.manual_seed_all(seed)
`,
	}
	for name, source := range fixtures {
		if err := os.WriteFile(name, []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	script := ascendV030RC2A3CompatScript
	for _, replacement := range [][2]string{
		{"/vllm-workspace/vllm-ascend/vllm_ascend/worker/model_runner_v1.py", runner},
		{"/vllm-workspace/vllm-ascend/vllm_ascend/worker/worker.py", worker},
		{"/vllm-workspace/vllm/vllm/utils/torch_utils.py", torchUtils},
	} {
		script = strings.ReplaceAll(script, replacement[0], replacement[1])
	}
	var generations []string
	for attempt := 0; attempt < 2; attempt++ {
		inspect := script + "\npython3 -c 'import os; print(os.environ[\"GPUWORKS_V1_RENDEZVOUS_GENERATION\"])'"
		out, err := exec.Command("sh", "-ec", inspect).CombinedOutput()
		if err != nil {
			t.Fatalf("pre-start attempt %d: %v\n%s", attempt+1, err, out)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		generation := strings.TrimSpace(lines[len(lines)-1])
		if len(generation) != 64 || strings.Trim(generation, "0123456789abcdef") != "" {
			t.Fatalf("pre-start generation = %q, want 64 hex characters", generation)
		}
		generations = append(generations, generation)
	}
	if generations[0] == generations[1] {
		t.Fatal("two pre-start runs reused the same rendezvous generation")
	}
	patched, err := os.ReadFile(worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"def _gpuworks_process_rendezvous(", "self._gpuworks_cpu_rendezvous(\"upstream-groups\")", "self.distributed_init_method, self.rank, self.parallel_config.world_size\n", "torch.npu.manual_seed(seed)"} {
		haystack := string(patched)
		if want == "torch.npu.manual_seed(seed)" {
			raw, readErr := os.ReadFile(torchUtils)
			if readErr != nil {
				t.Fatal(readErr)
			}
			haystack = string(raw)
		}
		if got := strings.Count(haystack, want); got != 1 {
			t.Fatalf("%q count = %d, want 1", want, got)
		}
	}
	for _, name := range []string{runner, worker, torchUtils} {
		if out, err := exec.Command("python3", "-m", "py_compile", name).CombinedOutput(); err != nil {
			t.Fatalf("patched %s does not compile: %v\n%s", name, err, out)
		}
	}
}

func TestAscendV2CompatibilityScriptPatchesKnownLayoutIdempotently(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "worker.py")
	const original = `import torch

class NPUWorker:
    def _init_device(self):
        self._init_worker_distributed_environment()
        # Set random seed.
        set_random_seed(self.model_config.seed)
        # Initialize device properties used by triton kernels.
        init_device_properties_triton()

        return device

    def init_device(self):
        self.device = self._init_device()
        # Init ModelRunner here, so that we have access to self.device.
        if self.use_v2_model_runner:
            self.model_runner = NPUModelRunnerV2(self.vllm_config, self.device)
`
	if err := os.WriteFile(worker, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(ascendV030RC2A3V2CompatScript,
		"/vllm-workspace/vllm-ascend/vllm_ascend/worker/worker.py", worker)
	for attempt := 0; attempt < 2; attempt++ {
		if out, err := exec.Command("sh", "-ec", script).CombinedOutput(); err != nil {
			t.Fatalf("pre-start attempt %d: %v\n%s", attempt+1, err, out)
		}
	}
	patched, err := os.ReadFile(worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GPUWorks: installed NPU zeros/ones CPU-copy fallback",
		"_gpuworks_faulthandler.register(_gpuworks_signal.SIGUSR1, all_threads=True)",
	} {
		if got := strings.Count(string(patched), want); got != 1 {
			t.Fatalf("%q count = %d, want 1", want, got)
		}
	}
	if strings.Contains(string(patched), "_gpuworks_coordinate_new_groups") || strings.Contains(string(patched), "GPUWorks: stack dump after stalled V2 model startup") {
		t.Fatalf("V2 script acquired V1 coordination or an automatic stack dump:\n%s", patched)
	}
	if out, err := exec.Command("python3", "-m", "py_compile", worker).CombinedOutput(); err != nil {
		t.Fatalf("patched worker does not compile: %v\n%s", err, out)
	}
}
