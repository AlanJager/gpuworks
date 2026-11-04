package udocker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

const platformLibraryPath = "/opt/nvidia-shim:/usr/local/nvidia/lib64:/usr/local/nvidia/lib:/usr/local/cuda/lib64"

// imageContainer materializes a container the way native create leaves one
// for an image built FROM ubuntu with `ENV LD_LIBRARY_PATH=...`: container.json
// is the image config blob, Env as the builder records it.
func imageContainer(t *testing.T, d *Driver, name string, env []string) {
	t.Helper()
	dir := d.containerDir(name)
	for _, sub := range []string{"ROOT/usr/local/bin", "ROOT/etc", "ROOT/root", "ROOT/workspace"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "ROOT/usr/local/bin/ninfer-serve"), []byte("\x7fELF"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ROOT/etc/passwd"), []byte("root:x:0:0:root:/root:/bin/bash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"architecture": "amd64",
		"os":           "linux",
		"created":      "2026-10-01T14:02:11.530Z",
		"config": map[string]any{
			"ExposedPorts": map[string]any{"8080/tcp": map[string]any{}},
			"Env":          env,
			"Cmd":          []string{"ninfer-serve", "--help"},
			"WorkingDir":   "/workspace",
			"Labels":       map[string]string{"org.opencontainers.image.ref.name": "ubuntu", "org.opencontainers.image.version": "24.04"},
		},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + strings.Repeat("ab", 32)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "container.json"), config, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ubuntuImageEnv(extra ...string) []string {
	return append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, extra...)
}

var libraryPathCases = []struct {
	name  string
	image []string
	spec  map[string]string
	want  string
	// imageOnly: neither the platform nor the spec sets the variable, so the
	// launcher takes the image value as is.
	imageOnly bool
}{
	{
		name:  "image library directory follows the platform path",
		image: ubuntuImageEnv("LD_LIBRARY_PATH=/usr/local/lib/ninfer", "NVIDIA_VISIBLE_DEVICES=all", "NVIDIA_DRIVER_CAPABILITIES=compute,utility"),
		want:  platformLibraryPath + ":/usr/local/lib/ninfer",
	},
	{
		name:  "image without a library path keeps the platform path",
		image: ubuntuImageEnv("NVIDIA_VISIBLE_DEVICES=all"),
		want:  platformLibraryPath,
	},
	{
		name:  "deployment value replaces platform and image paths",
		image: ubuntuImageEnv("LD_LIBRARY_PATH=/usr/local/lib/ninfer"),
		spec:  map[string]string{"LD_LIBRARY_PATH": "/srv/custom/lib"},
		want:  "/srv/custom/lib",
	},
	{
		name:  "duplicate and empty image entries are dropped",
		image: ubuntuImageEnv("LD_LIBRARY_PATH=:/usr/local/cuda/lib64::/usr/local/lib/ninfer:/opt/nvidia-shim/:"),
		want:  platformLibraryPath + ":/usr/local/lib/ninfer",
	},
	{
		name:      "domestic spec keeps the image value",
		image:     ubuntuImageEnv("LD_LIBRARY_PATH=/usr/local/lib/ninfer"),
		spec:      map[string]string{"HYGON_VISIBLE_DEVICES": "0"},
		want:      "/usr/local/lib/ninfer",
		imageOnly: true,
	},
	{
		name:  "domestic spec with injected driver path keeps it",
		image: ubuntuImageEnv("LD_LIBRARY_PATH=/usr/local/lib/ninfer"),
		spec:  map[string]string{"HYGON_VISIBLE_DEVICES": "0", "LD_LIBRARY_PATH": "/opt/hyhal/lib:/usr/local/lib/ninfer"},
		want:  "/opt/hyhal/lib:/usr/local/lib/ninfer",
	},
}

func libraryPathEntries(env []string) []string {
	var values []string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "LD_LIBRARY_PATH="); ok {
			values = append(values, value)
		}
	}
	return values
}

func assertSearchPath(t *testing.T, got []string, want string) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("LD_LIBRARY_PATH = %q, want exactly %q", got, want)
	}
	value := got[0]
	if strings.Contains(value, "::") || strings.HasPrefix(value, ":") || strings.HasSuffix(value, ":") {
		t.Fatalf("LD_LIBRARY_PATH %q has an empty entry", value)
	}
	seen := map[string]bool{}
	for _, dir := range strings.Split(value, ":") {
		if seen[dir] {
			t.Fatalf("LD_LIBRARY_PATH %q repeats %s", value, dir)
		}
		seen[dir] = true
	}
}

// The launch environment is what runveil.Prepare hands to the PRoot process,
// assembled through the same runveilCommand that Run, Exec and the terminal use.
func TestRunveilLaunchExtendsImageLibraryPath(t *testing.T) {
	for _, tc := range libraryPathCases {
		t.Run(tc.name, func(t *testing.T) {
			d := runveilDriver(t)
			imageContainer(t, d, "inst", tc.image)
			spec := runtime.WorkloadSpec{Name: "inst", Image: "local/ninfer:d44ab58", Env: tc.spec}
			command, _, err := d.runveilCommand(spec, nil, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			assertSearchPath(t, libraryPathEntries(command.Env), tc.want)
			if got := command.Argv[len(command.Argv)-2:]; got[0] != "ninfer-serve" || got[1] != "--help" {
				t.Fatalf("image command not launched: %q", command.Argv)
			}
			// The image's other variables are still filled in.
			if !strings.Contains(strings.Join(command.Env, "\n"), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") {
				t.Fatalf("image PATH lost: %q", command.Env)
			}
		})
	}
}

// An exec into a running container rebuilds the environment from the image
// as well; it carries no spec env at all.
func TestRunveilExecExtendsImageLibraryPath(t *testing.T) {
	d := runveilDriver(t)
	imageContainer(t, d, "inst", ubuntuImageEnv("LD_LIBRARY_PATH=/usr/local/lib/ninfer"))
	command, _, err := d.runveilCommand(runtime.WorkloadSpec{Name: "inst"}, []string{"/usr/local/bin/ninfer-serve"}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	assertSearchPath(t, libraryPathEntries(command.Env), platformLibraryPath+":/usr/local/lib/ninfer")
}

// The udocker CLI launcher passes the same environment as --env options, and
// udocker likewise fills in image Env only for keys not given that way.
func TestUdockerCLILaunchExtendsImageLibraryPath(t *testing.T) {
	for _, tc := range libraryPathCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(launcherEnv, "udocker")
			d := New(t.TempDir())
			imageContainer(t, d, "inst", tc.image)
			args := d.containerRunArgs(runtime.WorkloadSpec{Name: "inst", Env: tc.spec})
			var values []string
			for _, arg := range args {
				if value, ok := strings.CutPrefix(arg, "--env=LD_LIBRARY_PATH="); ok {
					values = append(values, value)
				}
			}
			if tc.imageOnly {
				// udocker reads the image value from container.json itself.
				if len(values) != 0 {
					t.Fatalf("domestic launch passed LD_LIBRARY_PATH %q", values)
				}
				return
			}
			assertSearchPath(t, values, tc.want)
		})
	}
}

func TestRunEnvWithoutContainerKeepsPlatformLibraryPath(t *testing.T) {
	d := New(t.TempDir())
	if got := d.runEnv(runtime.WorkloadSpec{Name: "not-created"})["LD_LIBRARY_PATH"]; got != platformLibraryPath {
		t.Fatalf("LD_LIBRARY_PATH = %q, want %q", got, platformLibraryPath)
	}
	// A malformed container.json is the launcher's error to report; the
	// environment falls back to the platform path.
	dir := d.containerDir("broken")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "container.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := d.runEnv(runtime.WorkloadSpec{Name: "broken"})["LD_LIBRARY_PATH"]; got != platformLibraryPath {
		t.Fatalf("LD_LIBRARY_PATH = %q, want %q", got, platformLibraryPath)
	}
}

// A ROCm workload pinned by ROCR_VISIBLE_DEVICES gets none of the NVIDIA shim
// environment: CUDA_VISIBLE_DEVICES=<host index> would hide the card from HIP,
// which numbers the cards it enumerates itself.
func TestRunEnvLeavesAMDSelectionToROCm(t *testing.T) {
	d := &Driver{}
	env := d.runEnv(runtime.WorkloadSpec{GPUIndexes: []int{1}, Env: map[string]string{"ROCR_VISIBLE_DEVICES": "GPU-bbdb6c523df10b7d"}})
	for _, key := range []string{"CUDA_VISIBLE_DEVICES", "TRITON_LIBCUDA_PATH", "NCCL_SOCKET_IFNAME", libraryPathEnv} {
		if _, ok := env[key]; ok {
			t.Fatalf("AMD runveil env carries %s=%q: %v", key, env[key], env)
		}
	}
	if env["ROCR_VISIBLE_DEVICES"] != "GPU-bbdb6c523df10b7d" {
		t.Fatalf("selection lost: %v", env)
	}
}
