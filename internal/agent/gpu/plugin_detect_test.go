package gpu

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestNvidiaDetectRuntimePluginReportsToolkit(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "nvidia-ctk")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho 'NVIDIA Container Toolkit CLI version 1.20.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := (&SMIProber{}).DetectRuntimePlugin(context.Background(), "docker")
	if got.Name != "NVIDIA Container Toolkit" || got.Version != "1.20.0" || !got.Installed {
		t.Fatalf("nvidia plugin = %+v", got)
	}
}

func TestNvidiaDetectRuntimePluginFailsClosedWhenToolkitMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := (&SMIProber{}).DetectRuntimePlugin(context.Background(), "nerdctl")
	if got.Name != "NVIDIA Container Toolkit" || got.Installed || got.Version != "" {
		t.Fatalf("nvidia plugin = %+v, want named-but-not-installed", got)
	}
}

func TestAscendDetectRuntimePluginIsDockerOnly(t *testing.T) {
	// nerdctl uses explicit device injection; no Docker-only plugin is reported.
	if got := (&AscendProber{}).DetectRuntimePlugin(context.Background(), "nerdctl"); got.Name != "" {
		t.Fatalf("ascend plugin under nerdctl = %+v, want empty", got)
	}
	dir := t.TempDir()
	tool := filepath.Join(dir, "ascend-docker-runtime")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\necho 'ascend-docker-runtime 8.5.0.alpha1'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	got := (&AscendProber{}).DetectRuntimePlugin(context.Background(), "docker")
	if got.Name != "Ascend Docker Runtime" || got.Version != "8.5.0.alpha1" || !got.Installed {
		t.Fatalf("ascend plugin = %+v", got)
	}
}
