package deploy

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
)

func TestKVCPUTierBytes(t *testing.T) {
	for name, test := range map[string]struct {
		size float64
		want int64
	}{
		"unset":                  {0, 0},
		"negative is no tier":    {-4, 0},
		"whole GiB":              {19, 19 << 30},
		"fractional GiB":         {0.5, 512 << 20},
		"large tier":             {800, 800 << 30},
		"beyond int64 saturates": {1e30, math.MaxInt64},
	} {
		t.Run(name, func(t *testing.T) {
			if got := KVCPUTierBytes(LaunchConfig{KVOffloadingSize: test.size}); got != test.want {
				t.Fatalf("KVCPUTierBytes(%v GiB) = %d, want %d", test.size, got, test.want)
			}
		})
	}
}

// tierBytesInArgv reads the CPU tier size the command asks the engine for: the
// connector's cpu_bytes_to_use inside --kv-transfer-config (disk tier, PD), or
// the GiB of --kv-offloading-size (memory tier alone).
func tierBytesInArgv(t *testing.T, argv []string) (int64, bool) {
	t.Helper()
	if size := argValue(t, argv, "--kv-offloading-size"); size != "" {
		gib, err := strconv.ParseFloat(size, 64)
		if err != nil {
			t.Fatalf("--kv-offloading-size %q: %v", size, err)
		}
		return int64(gib * float64(int64(1)<<30)), true
	}
	config := argValue(t, argv, "--kv-transfer-config")
	var doc struct {
		Extra struct {
			CPUBytes   *int64 `json:"cpu_bytes_to_use"`
			Connectors []struct {
				Extra struct {
					CPUBytes *int64 `json:"cpu_bytes_to_use"`
				} `json:"kv_connector_extra_config"`
			} `json:"connectors"`
		} `json:"kv_connector_extra_config"`
	}
	if config == "" {
		return 0, false
	}
	if err := json.Unmarshal([]byte(config), &doc); err != nil {
		t.Fatalf("--kv-transfer-config is not JSON: %v: %s", err, config)
	}
	if doc.Extra.CPUBytes != nil {
		return *doc.Extra.CPUBytes, true
	}
	for _, connector := range doc.Extra.Connectors {
		if connector.Extra.CPUBytes != nil {
			return *connector.Extra.CPUBytes, true
		}
	}
	return 0, false
}

// The connector's cpu_bytes_to_use and the container's /dev/shm come from the
// same reading of the tier: whatever the connector is told to allocate, the
// container is given exactly that much on top of the engine default.
func TestResolveLaunchShmCoversTheTierTheConnectorAllocates(t *testing.T) {
	base := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1}
	for name, test := range map[string]struct {
		quick     func(*LaunchConfig)
		pd        string
		wantBytes int64
	}{
		"memory tier":      {quick: func(q *LaunchConfig) { q.KVOffloadingSize = 19 }, wantBytes: 19 << 30},
		"fractional tier":  {quick: func(q *LaunchConfig) { q.KVOffloadingSize = 2.5 }, wantBytes: 2560 << 20},
		"disk tier":        {quick: func(q *LaunchConfig) { q.KVOffloadingSize, q.KVDiskOffload, q.SharedKVDir = 8, true, "/mnt/kv" }, wantBytes: 8 << 30},
		"prefill + memory": {quick: func(q *LaunchConfig) { q.KVOffloadingSize = 2 }, pd: "prefill", wantBytes: 2 << 30},
	} {
		t.Run(name, func(t *testing.T) {
			params := base
			params.Quick = configured(t, EngineVLLM, test.quick)
			if test.pd != "" {
				params = pdParams(test.pd, params.Quick, ComputeNetwork{})
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: params})
			if err != nil {
				t.Fatal(err)
			}
			cpuBytes, found := tierBytesInArgv(t, launchArgv(launch))
			if !found || cpuBytes != test.wantBytes {
				t.Fatalf("tier in argv = %d (found %v), want %d; argv = %v", cpuBytes, found, test.wantBytes, launch.Argv)
			}
			if got, want := launch.ShmBytes, (vLLMEngine{}).Profile().ShmBytes+cpuBytes; got != want {
				t.Fatalf("shm = %d, want the engine default plus the connector's %d bytes = %d", got, cpuBytes, want)
			}
		})
	}
}

func TestResolveLaunchShmDerivation(t *testing.T) {
	const profileShm = int64(16) << 30
	hostIPC := []string{"--ipc=host"}
	manual := ContainerOverrides{CommandMode: string(CommandManual), CmdDisplay: "vllm serve /model --port 30000"}
	argvOverride := ContainerOverrides{CmdDisplay: `["vllm","serve","/model"]`}
	for name, test := range map[string]struct {
		engine    string
		tierGiB   float64
		overrides ContainerOverrides
		want      int64
	}{
		"no tier keeps the engine default":      {engine: EngineVLLM, want: profileShm},
		"19 GiB tier adds to the default":       {engine: EngineVLLM, tierGiB: 19, want: profileShm + 19<<30},
		"explicit override wins":                {engine: EngineVLLM, tierGiB: 19, overrides: ContainerOverrides{ShmBytes: 64 << 30}, want: 64 << 30},
		"override exactly the tier":             {engine: EngineVLLM, tierGiB: 19, overrides: ContainerOverrides{ShmBytes: 19 << 30}, want: 19 << 30},
		"override below default but above tier": {engine: EngineVLLM, tierGiB: 4, overrides: ContainerOverrides{ShmBytes: 8 << 30}, want: 8 << 30},
		"override with no tier is unchecked":    {engine: EngineVLLM, overrides: ContainerOverrides{ShmBytes: 1 << 30}, want: 1 << 30},
		"host IPC states no size":               {engine: EngineVLLM, tierGiB: 19, overrides: ContainerOverrides{DockerArgs: hostIPC}, want: 0},
		// The tier a quick parameter left behind is not in effect when the
		// deployment owns the whole command.
		"manual command ignores the tier":    {engine: EngineVLLM, tierGiB: 19, overrides: withShm(manual, 8<<30), want: 8 << 30},
		"manual command takes the default":   {engine: EngineVLLM, tierGiB: 19, overrides: manual, want: profileShm},
		"complete argv ignores the tier":     {engine: EngineVLLM, tierGiB: 19, overrides: argvOverride, want: profileShm},
		"engine without the parameter":       {engine: EngineSGLang, tierGiB: 19, want: profileShm},
		"callable has no dictionary at all":  {engine: EngineCallable, tierGiB: 19, want: profileShm},
		"unrepresentable tier saturates":     {engine: EngineVLLM, tierGiB: 1e30, want: math.MaxInt64},
		"fractional tier is not rounded off": {engine: EngineVLLM, tierGiB: 0.5, want: profileShm + 512<<20},
	} {
		t.Run(name, func(t *testing.T) {
			engine, err := SelectEngine(test.engine)
			if err != nil {
				t.Fatal(err)
			}
			launch, err := ResolveLaunch(ResolveLaunchInput{
				Engine: engine, ContainerOverrides: test.overrides,
				Params: Params{
					ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, Quick: LaunchConfig{KVOffloadingSize: test.tierGiB},
					// A callable adapter's command is its template's; this one only
					// has to name the port the platform reserved.
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

// launchArgv is the whole command line of a launch, whichever half of it the
// engine put in the entrypoint.
func launchArgv(launch ResolvedLaunch) []string {
	return append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
}

func withShm(overrides ContainerOverrides, shmBytes int64) ContainerOverrides {
	overrides.ShmBytes = shmBytes
	return overrides
}

// An explicit size smaller than the tier can never start, so the launch fails
// at resolve time - where evaluate and the create preflight both reach it - and
// says which two sizes disagree.
func TestResolveLaunchRefusesShmBelowTheCPUTier(t *testing.T) {
	params := Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1, Quick: LaunchConfig{KVOffloadingSize: 19}}
	for name, test := range map[string]struct {
		shm               int64
		wantShm, wantTier float64
	}{
		"the engine default is not a safe size to type": {shm: 16 << 30, wantShm: 16, wantTier: 19},
		"just under the tier":                           {shm: 19<<30 - 1, wantShm: 18.99, wantTier: 19},
		"one GiB":                                       {shm: 1 << 30, wantShm: 1, wantTier: 19},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: params, ContainerOverrides: ContainerOverrides{ShmBytes: test.shm}})
			var productErr *messages.ProductError
			if !errors.As(err, &productErr) {
				t.Fatalf("ResolveLaunch() = %v, want a product error", err)
			}
			if productErr.Code() != messages.ErrorShmBelowKVCPUTier || productErr.Code() != "SHM_BELOW_KV_CPU_TIER" {
				t.Fatalf("code = %q", productErr.Code())
			}
			params := productErr.Params()
			if len(params) != 2 || params["shm_gib"] != test.wantShm || params["tier_gib"] != test.wantTier {
				t.Fatalf("params = %#v, want shm_gib %v and tier_gib %v", params, test.wantShm, test.wantTier)
			}
			if !strings.Contains(productErr.Error(), "CPU KV tier") {
				t.Fatalf("fallback = %q, want an English explanation", productErr.Error())
			}
		})
	}
	// The same override on a launch with no tier is the user's own business.
	if _, err := ResolveLaunch(ResolveLaunchInput{
		Engine: vLLMEngine{}, Params: Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1},
		ContainerOverrides: ContainerOverrides{ShmBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("override without a tier refused: %v", err)
	}
}

// KV offload belongs to the prefill role only: the decode role gets the plain
// engine default, because PDRoleLaunchConfig already stripped the tier from the
// configuration it resolves with, and an override that fits the decode role
// still has to fit the prefill role the same deployment launches.
func TestResolveLaunchShmForPDRoles(t *testing.T) {
	tier := LaunchConfig{KVOffloadingSize: 19}
	resolve := func(role string, overrides ContainerOverrides) (ResolvedLaunch, error) {
		return ResolveLaunch(ResolveLaunchInput{
			Engine: vLLMEngine{}, Params: pdParams(role, tier, ComputeNetwork{}), ContainerOverrides: overrides,
		})
	}
	prefill, err := resolve("prefill", ContainerOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(16+19) << 30; prefill.ShmBytes != want {
		t.Fatalf("prefill shm = %d, want default + tier = %d", prefill.ShmBytes, want)
	}
	decode, err := resolve("decode", ContainerOverrides{})
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(16) << 30; decode.ShmBytes != want {
		t.Fatalf("decode shm = %d, want the plain engine default %d", decode.ShmBytes, want)
	}
	if size, found := tierBytesInArgv(t, launchArgv(decode)); found {
		t.Fatalf("decode argv carries a CPU tier of %d bytes: %v", size, decode.Argv)
	}

	small := ContainerOverrides{ShmBytes: 8 << 30}
	if _, err := resolve("prefill", small); err == nil {
		t.Fatal("a prefill role with a 19 GiB tier accepted an 8 GiB /dev/shm")
	} else {
		var productErr *messages.ProductError
		if !errors.As(err, &productErr) || productErr.Code() != messages.ErrorShmBelowKVCPUTier {
			t.Fatalf("prefill err = %v, want %s", err, messages.ErrorShmBelowKVCPUTier)
		}
	}
	got, err := resolve("decode", small)
	if err != nil || got.ShmBytes != 8<<30 {
		t.Fatalf("decode with the same override = %d, %v; want the override, which its stripped config does not contradict", got.ShmBytes, err)
	}
}
