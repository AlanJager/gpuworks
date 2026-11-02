package udocker

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestHygonRunveilLaunchBindsDriverAndPreservesImageSearchPath(t *testing.T) {
	d := runveilDriver(t)
	imagePath := "/opt/dtk/lib:/opt/hyhal/lib:/usr/local/lib"
	imageContainer(t, d, "hygon", ubuntuImageEnv("LD_LIBRARY_PATH="+imagePath))
	driverDir := t.TempDir()
	spec := runtime.WorkloadSpec{
		Name:   "hygon",
		Env:    map[string]string{"HYGON_VISIBLE_DEVICES": "0", "HIP_VISIBLE_DEVICES": "0"},
		Mounts: []runtime.Mount{{Source: driverDir, Target: "/opt/hyhal", ReadOnly: true}},
	}
	command, _, err := d.runveilCommand(spec, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Clean(driverDir) + ":/opt/hyhal"
	index := slices.Index(command.Argv, want)
	if index <= 0 || command.Argv[index-1] != "-b" {
		t.Fatalf("driver directory not bound in PRoot argv: %q", command.Argv)
	}
	assertSearchPath(t, libraryPathEntries(command.Env), imagePath)
	if !slices.Contains(command.Env, "HIP_VISIBLE_DEVICES=0") {
		t.Fatalf("device selection missing: %q", command.Env)
	}
}

func TestHygonPrivateConfigAndLogDirectoriesReachBothLaunchers(t *testing.T) {
	for _, launcher := range []string{"runveil", "udocker"} {
		t.Run(launcher, func(t *testing.T) {
			d := runveilDriver(t)
			d.runveil = launcher == "runveil"
			t.Setenv(launcherEnv, launcher)
			var views, logs []string
			for index, name := range []string{"first", "second"} {
				imageContainer(t, d, name, ubuntuImageEnv("LD_LIBRARY_PATH=/image/dtk/lib"))
				instance := t.TempDir()
				view, log := filepath.Join(instance, "view"), filepath.Join(instance, "hylog")
				for _, path := range []string{filepath.Join(view, "docker"), log} {
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				filename := fmt.Sprintf("vdev%d.conf", index+4)
				for _, path := range []string{filepath.Join(view, filename), filepath.Join(view, "docker", filename)} {
					if err := os.WriteFile(path, []byte(fmt.Sprintf("vdev_id: %d\nmem: 1536 MiB\n", index+4)), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				spec := runtime.WorkloadSpec{
					Name: name,
					Env:  map[string]string{"HYGON_VISIBLE_DEVICES": "0", "HIP_VISIBLE_DEVICES": "0"},
					Mounts: []runtime.Mount{
						{Source: view, Target: "/etc/vdev", ReadOnly: true},
						{Source: log, Target: "/var/log/hylog"},
					},
				}
				var args, env []string
				if d.runveil {
					command, _, err := d.runveilCommand(spec, nil, nil, "")
					if err != nil {
						t.Fatal(err)
					}
					args, env = command.Argv, command.Env
					for _, path := range []string{view + ":/etc/vdev", log + ":/var/log/hylog"} {
						if at := slices.Index(args, path); at <= 0 || args[at-1] != "-b" {
							t.Fatalf("runveil private mount missing: %s in %v", path, args)
						}
					}
				} else {
					args = d.containerRunArgs(spec)
					for _, path := range []string{"--volume=" + view + ":/etc/vdev", "--volume=" + log + ":/var/log/hylog"} {
						if !slices.Contains(args, path) {
							t.Fatalf("legacy udocker private mount missing: %s in %v", path, args)
						}
					}
					for key, value := range d.runEnv(spec) {
						env = append(env, key+"="+value)
					}
				}
				if !slices.Contains(env, "HIP_VISIBLE_DEVICES=0") || !slices.Contains(env, "HYGON_VISIBLE_DEVICES=0") {
					t.Fatalf("Hygon selection missing: %v", env)
				}
				if joined := strings.Join(args, " "); strings.Contains(joined, "vdev-id") || strings.Contains(joined, "/etc/vdev/vdev0.conf") || strings.Contains(joined, "--tmpfs") {
					t.Fatalf("ledger/fixed-ID alias/Docker-only log preparation leaked: %v", args)
				}
				views, logs = append(views, view), append(logs, log)
			}
			if views[0] == views[1] || logs[0] == logs[1] {
				t.Fatalf("two workloads share config or log directory: %v/%v", views, logs)
			}
		})
	}
}
