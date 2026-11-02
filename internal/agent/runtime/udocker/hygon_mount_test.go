package udocker

import (
	"path/filepath"
	"slices"
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
