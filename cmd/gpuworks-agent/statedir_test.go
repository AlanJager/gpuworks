package main

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"

	"gpuworks.ai/gpuworks/internal/agentconf"
)

// On a Mac ~/gpuworks is the agent's state only once an install configured
// it there: the control plane installs into ~/gpuworks/server as well, and an
// agent an earlier install left in ~/.gpuworks keeps being found.
func TestDefaultStateDirOnAMac(t *testing.T) {
	if goruntime.GOOS != "darwin" {
		t.Skip("the home-directory rule is macOS's")
	}
	home := t.TempDir()
	defer func(dir string) { systemStateDir = dir }(systemStateDir)
	systemStateDir = filepath.Join(home, "no-opt")
	t.Setenv("HOME", home)
	t.Setenv("GPUWORKS_STATE_DIR", "")

	current, legacy := filepath.Join(home, "gpuworks"), filepath.Join(home, ".gpuworks")
	if err := os.MkdirAll(filepath.Join(current, "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := defaultStateDir(); got != legacy {
		t.Fatalf("with only a control plane in ~/gpuworks, state = %q, want the earlier %s", got, legacy)
	}
	if err := os.WriteFile(agentconf.Path(current), []byte("TOKEN=t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := defaultStateDir(); got != current {
		t.Fatalf("state = %q, want the configured %s", got, current)
	}
}
