// Package extplugin holds the contract tests that run real plugin
// processes (ADR 0071, decision 21) and the helpers they share: building
// the fake plugin (./fakeplugin), and letting a test binary act as the
// server's plugin-exec trampoline.
package extplugin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/pluginexec"
)

// FakePluginPackage is the import path of the fake plugin binary.
const FakePluginPackage = "gpuworks.ai/gpuworks/internal/testenv/extplugin/fakeplugin"

// FakeManifest is the manifest digest the fake plugin answers with unless
// told otherwise; a Spec for it names the same.
const FakeManifest = "sha256:69a8fc5cf9fa0174e3dfe74b1c9c1f5f4f64710afa362419883f19c6b42ee422"

// RunTrampolineIfAsked makes a test binary the trampoline: when it was
// started as `<test binary> plugin-exec ...` it runs pluginexec and exits.
// Call it first in TestMain and point the plugin host's Trampoline at
// os.Executable().
func RunTrampolineIfAsked() {
	if len(os.Args) > 1 && os.Args[1] == pluginexec.Command {
		os.Exit(pluginexec.Main(os.Args[2:]))
	}
}

// BuildFakePlugin builds the fake plugin as a static binary in dir (its
// package directory) and returns the binary's path.
func BuildFakePlugin(dir string) (string, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("building the fake plugin needs the go command: %w", err)
	}
	out := filepath.Join(dir, "fakeplugin")
	cmd := exec.Command(goBin, "build", "-o", out, FakePluginPackage)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s: %v\n%s", FakePluginPackage, err, b)
	}
	return out, nil
}
