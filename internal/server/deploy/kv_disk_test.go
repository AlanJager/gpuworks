package deploy

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

const testGiB = int64(1) << 30

// kvDiskLimitSpec finds the published dictionary row, if the engine has one.
func kvDiskLimitSpec(t *testing.T, engine string) (ParamDefinition, bool) {
	t.Helper()
	profile, err := ProfileForEngine(engine)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range profile.ParamDefinitions {
		if spec.Key == "kv_disk_limit_gib" {
			return spec, true
		}
	}
	return ParamDefinition{}, false
}

// A platform parameter is a first-class dictionary row that names no engine
// flag, because none exists (ADR 0021 decision 1 and 2). The published row has
// to say exactly that: an empty flag, an explicit engine list, and no entry in
// the key→flag map, which is the map's whole meaning.
func TestKVDiskLimitIsPublishedAsAPlatformParameter(t *testing.T) {
	spec, published := kvDiskLimitSpec(t, EngineVLLM)
	if !published {
		t.Fatal("vLLM does not publish kv_disk_limit_gib")
	}
	if spec.Flag != "" || spec.FlagOff != "" {
		t.Fatalf("platform parameter advertises a flag: %#v", spec)
	}
	if !slices.Equal(spec.Engines, []string{EngineVLLM}) {
		t.Fatalf("engines = %v, want [vllm]", spec.Engines)
	}
	if !spec.Automatic || spec.Type != "number" || spec.Group != "performance" {
		t.Fatalf("published row = %#v", spec)
	}
	profile, err := ProfileForEngine(EngineVLLM)
	if err != nil {
		t.Fatal(err)
	}
	if flag, present := profile.ParamDict["kv_disk_limit_gib"]; present {
		t.Fatalf("param_dict maps the platform parameter to %q; it owns no flag", flag)
	}
	if _, publishedForSGLang := kvDiskLimitSpec(t, EngineSGLang); publishedForSGLang {
		t.Fatal("sglang publishes kv_disk_limit_gib, which lists only vllm")
	}
}

// ADR 0021 decision 3: what the deployment page shows is what the container
// runs. A platform parameter with a value must leave no trace in either.
func TestKVDiskLimitNeverReachesTheCommand(t *testing.T) {
	quick := configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.KVOffloadingSize, q.KVDiskOffload, q.SharedKVDir = 8, true, "/mnt/kv"
		q.KVDiskLimitGiB = 37
	})
	params := Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: quick}
	built := vLLMEngine{}.Build(params)
	preview := append(append([]string(nil), built.Entrypoint...), built.Argv...)
	for _, token := range preview {
		if strings.Contains(token, "kv_disk_limit_gib") || strings.Contains(token, "37") {
			t.Fatalf("the platform parameter leaked into the command: %v", preview)
		}
	}
	// The preview is the argv, token for token - the two are the same list, so
	// a user can copy what the page shows and run it.
	launch, err := ResolveLaunch(ResolveLaunchInput{Engine: vLLMEngine{}, Params: params})
	if err != nil {
		t.Fatal(err)
	}
	container := append(append([]string(nil), launch.Entrypoint...), launch.Argv...)
	if !slices.Equal(preview, container) {
		t.Fatalf("preview %v differs from container argv %v", preview, container)
	}
	// The disk tier itself is still configured; only the ceiling is invisible.
	if !slices.Contains(container, "--kv-transfer-config") {
		t.Fatalf("the disk tier flag is missing from %v", container)
	}
}

// The write boundary collects a platform parameter like any other stated value,
// and the argv reader can never produce it because no flag carries it.
func TestKVDiskLimitMaterializesAndNeverReverses(t *testing.T) {
	out, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"kv_disk_limit_gib": 12}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Quick.KVDiskLimitGiB != 12 {
		t.Fatalf("materialized limit = %v, want 12", out.Quick.KVDiskLimitGiB)
	}
	for _, argv := range [][]string{
		{"--kv-transfer-config", `{"kv_connector":"OffloadingConnector"}`},
		{"--kv-disk-limit-gib", "12"},
		{"--kv_disk_limit_gib=12"},
	} {
		params, residual, err := ReverseArgs(EngineVLLM, argv)
		if err != nil {
			t.Fatal(err)
		}
		if _, produced := params["kv_disk_limit_gib"]; produced {
			t.Fatalf("ReverseArgs(%v) produced the platform parameter", argv)
		}
		if !slices.Equal(residual, argv) {
			t.Fatalf("ReverseArgs(%v) residual = %v, want the tokens verbatim", argv, residual)
		}
	}
}

// The ceiling is a disk-tier setting even though no engine flag carries it.
// A submitted child without its enabled parent is rejected so a hidden draft
// cannot silently affect the agent sweeper.
func TestKVDiskLimitValidationRequiresTheDiskTier(t *testing.T) {
	for name, test := range map[string]struct {
		limit   float64
		disk    bool
		wantErr string
	}{
		"unset is a deployer who chose nothing":     {limit: 0},
		"a limit without the disk tier is rejected": {limit: 16, wantErr: "kv_disk_limit_gib requires kv_disk_offload"},
		"a limit with the disk tier is kept":        {limit: 16, disk: true},
		"a negative limit is rejected":              {limit: -1, wantErr: "must not be negative"},
		"a limit below the minimum is rejected":     {limit: 0.5, wantErr: "must be at least 1"},
	} {
		t.Run(name, func(t *testing.T) {
			quick := configured(t, EngineVLLM, func(q *LaunchConfig) {
				q.KVDiskLimitGiB = test.limit
				if test.disk {
					q.KVOffloadingSize, q.KVDiskOffload, q.SharedKVDir = 8, true, "/mnt/kv"
				}
			})
			err := ValidateLaunchConfigForEngine(EngineVLLM, quick)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateLaunchConfigForEngine() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ValidateLaunchConfigForEngine() = %v, want an error containing %q", err, test.wantErr)
			}
		})
	}
	// A stated zero is a real zero at the write boundary, where presence is
	// still visible, and is refused there rather than stored as "unset".
	if _, err := Materialize(MaterializeInput{Engine: EngineVLLM, Raw: map[string]any{"kv_disk_limit_gib": 0}}); err == nil {
		t.Fatal("Materialize accepted an explicit kv_disk_limit_gib of 0")
	}
}

func TestResolveLaunchMountsTheDiskTierDirectory(t *testing.T) {
	for name, test := range map[string]struct {
		limitGiB     float64
		weightBytes  int64
		sharedKVDir  string
		wantMount    bool
		wantSizeHint string
	}{
		"no directory means no managed mount": {},
		"a stated ceiling is carried verbatim": {
			sharedKVDir: "/mnt/kv", limitGiB: 4, weightBytes: 10 * testGiB,
			wantMount: true, wantSizeHint: strconv.FormatInt(4*testGiB, 10),
		},
		"an unstated ceiling is twice the weights": {
			sharedKVDir: "/mnt/kv", weightBytes: 10 * testGiB,
			wantMount: true, wantSizeHint: strconv.FormatInt(20*testGiB, 10),
		},
		"a preview with no sized model carries no ceiling": {
			sharedKVDir: "/mnt/kv", wantMount: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			quick := configured(t, EngineVLLM, func(q *LaunchConfig) {
				q.KVDiskLimitGiB = test.limitGiB
				if test.sharedKVDir != "" {
					q.KVOffloadingSize, q.KVDiskOffload, q.SharedKVDir = 8, true, test.sharedKVDir
				}
			})
			launch, err := ResolveLaunch(ResolveLaunchInput{
				Engine: vLLMEngine{},
				Params: Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: quick},
				// The disk tier mount is the deployment's, so it must not be
				// confused with a mount the image or the user asked for.
				ContainerOverrides: overridesWithMount("/opt/extra"),
				ModelWeightBytes:   test.weightBytes,
			})
			if err != nil {
				t.Fatal(err)
			}
			var found []proto.MountSpec
			for _, mount := range launch.Mounts {
				if mount.Purpose == proto.MountPurposeKVDisk {
					found = append(found, mount)
				}
			}
			if !test.wantMount {
				if len(found) != 0 {
					t.Fatalf("mounts = %#v, want no kv-disk mount", launch.Mounts)
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("kv-disk mounts = %#v, want exactly one", found)
			}
			mount := found[0]
			if mount.Target != "/mnt/kv" || mount.Source != "" || mount.ReadOnly {
				t.Fatalf("mount = %#v; the agent decides the host source and the tier writes to it", mount)
			}
			if mount.SizeHint != test.wantSizeHint {
				t.Fatalf("size hint = %q, want %q", mount.SizeHint, test.wantSizeHint)
			}
		})
	}
}

// A user mount on the tier's container path is a real conflict, and the deployer
// has to be able to read what to change.
func TestResolveLaunchReportsAMountTargetConflict(t *testing.T) {
	quick := configured(t, EngineVLLM, func(q *LaunchConfig) {
		q.KVOffloadingSize, q.KVDiskOffload, q.SharedKVDir = 8, true, "/mnt/kv"
	})
	_, err := ResolveLaunch(ResolveLaunchInput{
		Engine:             vLLMEngine{},
		Params:             Params{ModelPath: "/model", APIPort: 30000, TensorParallel: 1, AssignedGPUs: 1, Quick: quick},
		ContainerOverrides: overridesWithMount("/mnt/kv"),
	})
	var productErr *messages.ProductError
	if !errors.As(err, &productErr) {
		t.Fatalf("ResolveLaunch() = %v, want a product error", err)
	}
	if productErr.Code() != messages.ErrorMountTargetDuplicate {
		t.Fatalf("code = %q, want %q", productErr.Code(), messages.ErrorMountTargetDuplicate)
	}
	if productErr.Params()["target"] != "/mnt/kv" {
		t.Fatalf("params = %#v, want the conflicting target", productErr.Params())
	}
}

// The wire form of the configuration carries the platform parameter under the
// dictionary's key, and a deployment that never stated one carries nothing.
func TestKVDiskLimitWireJSON(t *testing.T) {
	encoded, err := json.Marshal(LaunchConfig{KVDiskLimitGiB: 12})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"kv_disk_limit_gib":12`) {
		t.Fatalf("encoded = %s", encoded)
	}
	if encoded, err = json.Marshal(LaunchConfig{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "kv_disk_limit_gib") {
		t.Fatalf("an unset ceiling reached the wire: %s", encoded)
	}
	var decoded LaunchConfig
	if err := json.Unmarshal([]byte(`{"kv_disk_limit_gib":2.5}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.KVDiskLimitGiB != 2.5 {
		t.Fatalf("decoded = %#v", decoded)
	}
}

// overridesWithMount is a deployment that already mounts a container path, the
// way a user-supplied mount row reaches launch resolution.
func overridesWithMount(containerPath string) ContainerOverrides {
	return ContainerOverrides{ExtraMounts: []ContainerMount{{HostPath: "/srv/data", ContainerPath: containerPath}}}
}
