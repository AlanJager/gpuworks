package engine

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestCheckModelPathsAcceptsDirectories(t *testing.T) {
	dir := t.TempDir()
	component := filepath.Join(dir, "component")
	if err := os.Mkdir(component, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := proto.InstanceDesiredState{ModelPath: dir}
	spec.Mounts = []proto.MountSpec{{Source: component, Purpose: "model_component:vision"}, {Source: "/nonexistent-other", Purpose: "kv_disk"}}
	if err := checkModelPaths(spec); err != nil {
		t.Fatalf("check = %v", err)
	}
}

func TestCheckModelPathsRejectsMissingFileAndComponent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "weights.bin")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	component := proto.InstanceDesiredState{ModelPath: dir}
	component.Mounts = []proto.MountSpec{{Source: filepath.Join(dir, "absent"), Purpose: "model_component:vision"}}
	for name, spec := range map[string]proto.InstanceDesiredState{
		"missing":   {ModelPath: filepath.Join(dir, "absent")},
		"file":      {ModelPath: file},
		"component": component,
	} {
		err := checkModelPaths(spec)
		if err == nil || !strings.HasPrefix(err.Error(), ModelPathUnavailable) {
			t.Fatalf("%s: err = %v, want %s", name, err, ModelPathUnavailable)
		}
	}
}

func TestCheckModelPathsGivesUpOnAHungFilesystem(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hung := func(string) (fs.FileInfo, error) { <-release; return nil, os.ErrNotExist }
	start := time.Now()
	err := checkModelPathsWith(proto.InstanceDesiredState{ModelPath: "/mnt/gpuworks/hung/model"}, hung)
	if err == nil || !strings.Contains(err.Error(), "did not answer in time") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > modelPathCheckTimeout+time.Second {
		t.Fatalf("check took %s", elapsed)
	}
}
