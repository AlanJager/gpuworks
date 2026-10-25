package deploy

import (
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const engramTestConfig = `{"cpu_offload":true,"dp_shared_memory":true}`

func engramLaunchInput(runtime string) ResolveLaunchInput {
	return ResolveLaunchInput{
		Engine: vLLMEngine{},
		Params: Params{
			ModelPath: "/model", APIPort: 30000,
			AssignedGPUs: 16, TensorParallel: 4, Quick: LaunchConfig{DP: 4},
			TemplateArgs: []string{"--engram-config", engramTestConfig}, RuntimeDriver: runtime,
		},
	}
}

func engramCapabilityCount(args []string) int {
	count := 0
	for index, arg := range args {
		if dockerOptionName(strings.TrimSpace(arg)) == "--cap-add" && strings.EqualFold(dockerOptionValue(args, index), "IPC_LOCK") {
			count++
		}
	}
	return count
}

func TestResolveLaunchDefaultsEngramSharedMemory(t *testing.T) {
	for _, runtime := range []string{"", proto.RuntimePreferenceDocker, proto.RuntimePreferenceNerdctl} {
		t.Run(runtime, func(t *testing.T) {
			input := engramLaunchInput(runtime)
			input.TemplateMemlockUnlimited = true
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if launch.ShmBytes != engramSharedMemoryDefaultBytes {
				t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
			}
			if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, []string{"memlock=-1:-1"}) {
				t.Fatalf("memlock args = %v in %v, want one unlimited limit", got, launch.DockerArgs)
			}
			if got := engramCapabilityCount(launch.DockerArgs); got != 1 {
				t.Fatalf("IPC_LOCK count = %d in %v", got, launch.DockerArgs)
			}
		})
	}
}

func TestResolveLaunchEngramSharedMemoryRules(t *testing.T) {
	const tier = int64(19) << 30
	for name, test := range map[string]struct {
		config        string
		quick         LaunchConfig
		overrides     ContainerOverrides
		templateBytes int64
		wantShm       int64
		wantLock      bool
	}{
		"cpu offload without DP sharing keeps the engine default": {
			config:  `{"cpu_offload":true,"dp_shared_memory":false}`,
			wantShm: (vLLMEngine{}).Profile().ShmBytes,
		},
		"CPU tier adds to the Engram baseline": {
			config:   engramTestConfig,
			quick:    LaunchConfig{KVOffloadingSize: 19},
			wantShm:  engramSharedMemoryDefaultBytes + tier,
			wantLock: true,
		},
		"explicit shared memory is preserved": {
			config:    engramTestConfig,
			overrides: ContainerOverrides{ShmBytes: 256 << 30},
			wantShm:   256 << 30,
			wantLock:  true,
		},
		"host IPC states no size": {
			config:    engramTestConfig,
			overrides: ContainerOverrides{DockerArgs: []string{"--ipc=host"}},
			wantShm:   0,
			wantLock:  true,
		},
		"template remains a total floor": {
			config:        engramTestConfig,
			templateBytes: 1024 << 30,
			quick:         LaunchConfig{KVOffloadingSize: 19},
			wantShm:       1024 << 30,
			wantLock:      true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceDocker)
			input.Params.Quick.DP = 4
			input.Params.Quick.KVOffloadingSize = test.quick.KVOffloadingSize
			input.Params.TemplateArgs = []string{"--engram-config", test.config}
			input.ContainerOverrides = test.overrides
			input.TemplateShmBytes = test.templateBytes
			launch, err := ResolveLaunch(input)
			if err != nil {
				t.Fatal(err)
			}
			if launch.ShmBytes != test.wantShm {
				t.Fatalf("shm = %d, want %d", launch.ShmBytes, test.wantShm)
			}
			if got := len(memlockArgs(launch.DockerArgs)) > 0; got != test.wantLock {
				t.Fatalf("memlock present = %v, want %v; args = %v", got, test.wantLock, launch.DockerArgs)
			}
			if got := engramCapabilityCount(launch.DockerArgs) > 0; got != test.wantLock {
				t.Fatalf("IPC_LOCK present = %v, want %v; args = %v", got, test.wantLock, launch.DockerArgs)
			}
		})
	}
}

func TestEngramKeepsExplicitMemlockAndCapabilitySpellings(t *testing.T) {
	for _, args := range [][]string{
		{"--ulimit", "memlock=4096", "--cap-add=IPC_LOCK"},
		{"--ulimit=memlock=4096", "--cap-add", "IPC_LOCK"},
		{"--ulimit memlock=4096", "--cap-add IPC_LOCK"},
	} {
		input := engramLaunchInput(proto.RuntimePreferenceDocker)
		input.TemplateMemlockUnlimited = true
		input.ContainerOverrides.DockerArgs = args
		launch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatal(err)
		}
		if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, []string{"memlock=4096"}) {
			t.Fatalf("memlock args = %v in %v", got, launch.DockerArgs)
		}
		if got := engramCapabilityCount(launch.DockerArgs); got != 1 {
			t.Fatalf("IPC_LOCK count = %d in %v", got, launch.DockerArgs)
		}
	}
}

func TestEngramSharedMemoryRuntimeWarns(t *testing.T) {
	t.Run("udocker warns and still gets the resources", func(t *testing.T) {
		launch, warnings := resolveWithWarnings(t, engramLaunchInput(proto.RuntimePreferenceUdocker))
		requireWarningCodes(t, warnings, messages.WarningEngramSharedMemoryRuntime)
		params := warnings[0].Params
		if params["runtime"] != proto.RuntimePreferenceUdocker || params["flag"] != "--engram-config" || params["shm_gib"] != int64(512) {
			t.Fatalf("warning params = %#v", params)
		}
		if launch.ShmBytes != engramSharedMemoryDefaultBytes {
			t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
		}
	})
	for _, runtime := range []string{"", proto.RuntimePreferenceDocker, proto.RuntimePreferenceNerdctl} {
		t.Run("silent on "+runtime, func(t *testing.T) {
			_, warnings := resolveWithWarnings(t, engramLaunchInput(runtime))
			requireWarningCodes(t, warnings)
		})
	}
	t.Run("ordinary udocker is unchanged", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.TemplateArgs = nil
		launch, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings)
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
			t.Fatalf("ordinary udocker resources changed: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
		}
	})
}

func TestEngramDetectionUsesEffectiveArgv(t *testing.T) {
	t.Run("a complete override removes the template config", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceUdocker)
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = `["vllm","serve","/model"]`
		launch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatal(err)
		}
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
			t.Fatalf("removed Engram config still changed resources: %+v", launch)
		}
	})
	t.Run("a complete override adds the config", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.TemplateArgs = nil
		input.ContainerOverrides.CommandMode = string(CommandManual)
		input.ContainerOverrides.CmdDisplay = `["vllm","serve","/model","--engram-config","{\"cpu_offload\":true,\"dp_shared_memory\":true}"]`
		_, warnings := resolveWithWarnings(t, input)
		requireWarningCodes(t, warnings, messages.WarningEngramSharedMemoryRuntime)
	})
}

func TestEngramSharedMemoryArgvForms(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		want bool
	}{
		"separate value":                  {args: []string{"--engram-config", engramTestConfig}, want: true},
		"equals value":                    {args: []string{"--engram-config=" + engramTestConfig}, want: true},
		"underscore separate":             {args: []string{"--engram_config", engramTestConfig}, want: true},
		"underscore equals":               {args: []string{"--engram_config=" + engramTestConfig}, want: true},
		"raw DP uses CPU offload default": {args: []string{"--engram-config", `{"dp_shared_memory":true}`}, want: true},
		"dotted separate values": {args: []string{
			"--engram-config.cpu_offload", "true", "--engram-config.dp_shared_memory", "true",
		}, want: true},
		"dotted equals values": {args: []string{
			"--engram-config.cpu_offload=true", "--engram-config.dp_shared_memory=true",
		}, want: true},
		"dotted DP uses CPU offload default": {args: []string{"--engram-config.dp_shared_memory=true"}, want: true},
		"underscore dotted root":             {args: []string{"--engram_config.dp_shared_memory=true"}, want: true},
		"disabled DP":                        {args: []string{"--engram-config", `{"cpu_offload":true,"dp_shared_memory":false}`}},
		"malformed JSON":                     {args: []string{"--engram-config", `{cpu_offload:true}`}},
		"missing value":                      {args: []string{"--engram-config"}},
		"null config":                        {args: []string{"--engram-config", "null"}},
		"absent":                             {args: []string{"--max-model-len", "4096"}},
		"dotted group replaces earlier raw": {args: []string{
			"--engram-config", engramTestConfig, "--engram-config.cpu_offload=false",
		}},
		"dotted group replaces later raw": {args: []string{
			"--engram-config.dp_shared_memory=true", "--engram-config", `{"cpu_offload":true,"dp_shared_memory":false}`,
		}, want: true},
		"raw CPU false is replaced by dotted group": {args: []string{
			"--engram-config", `{"cpu_offload":false}`, "--engram-config.dp_shared_memory=true",
		}, want: true},
		"last dotted field wins": {args: []string{
			"--engram-config.dp_shared_memory=true", "--engram_config.dp_shared_memory=false",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := engramSharesHostMemory(test.args); got != test.want {
				t.Fatalf("engramSharesHostMemory(%v) = %v, want %v", test.args, got, test.want)
			}
		})
	}
}

func TestEngramDottedAndUncertainRuntimeWarnings(t *testing.T) {
	for name, test := range map[string]struct {
		args []string
		code string
	}{
		"underscore raw":       {[]string{"--engram_config", engramTestConfig}, messages.WarningEngramSharedMemoryRuntime},
		"dotted":               {[]string{"--engram-config.dp_shared_memory=true"}, messages.WarningEngramSharedMemoryRuntime},
		"underscore dotted":    {[]string{"--engram_config.cpu_offload=true", "--engram_config.dp_shared_memory=true"}, messages.WarningEngramSharedMemoryRuntime},
		"malformed raw":        {[]string{"--engram-config", `{cpu_offload:true}`}, messages.WarningEngramConfigUncertain},
		"missing dotted value": {[]string{"--engram-config.dp_shared_memory"}, messages.WarningEngramConfigUncertain},
	} {
		t.Run(name, func(t *testing.T) {
			input := engramLaunchInput(proto.RuntimePreferenceUdocker)
			input.Params.TemplateArgs = test.args
			_, warnings := resolveWithWarnings(t, input)
			requireWarningCodes(t, warnings, test.code)
		})
	}
	t.Run("valid disabled dotted config remains allowed", func(t *testing.T) {
		input := engramLaunchInput(proto.RuntimePreferenceUdocker)
		input.Params.TemplateArgs = []string{"--engram-config.dp_shared_memory=false"}
		launch, err := ResolveLaunch(input)
		if err != nil {
			t.Fatal(err)
		}
		if launch.ShmBytes != (vLLMEngine{}).Profile().ShmBytes || len(launch.DockerArgs) != 0 {
			t.Fatalf("disabled dotted Engram changed resources: shm=%d args=%v", launch.ShmBytes, launch.DockerArgs)
		}
	})
}

func TestResolveLaunchDottedEngramGetsSharedMemoryResources(t *testing.T) {
	input := engramLaunchInput(proto.RuntimePreferenceDocker)
	input.Params.TemplateArgs = []string{"--engram_config.dp_shared_memory=true"}
	launch, err := ResolveLaunch(input)
	if err != nil {
		t.Fatal(err)
	}
	if launch.ShmBytes != engramSharedMemoryDefaultBytes {
		t.Fatalf("shm = %d, want %d", launch.ShmBytes, engramSharedMemoryDefaultBytes)
	}
	if got := memlockArgs(launch.DockerArgs); !reflect.DeepEqual(got, []string{"memlock=-1:-1"}) {
		t.Fatalf("memlock args = %v in %v", got, launch.DockerArgs)
	}
	if got := engramCapabilityCount(launch.DockerArgs); got != 1 {
		t.Fatalf("IPC_LOCK count = %d in %v", got, launch.DockerArgs)
	}
}
