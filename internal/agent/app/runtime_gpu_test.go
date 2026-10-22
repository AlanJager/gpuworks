package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/agent/runtime/nerdctl"
)

// namedDriver is a container runtime driver that only has to say its name.
type namedDriver struct {
	runtime.Driver
	name string
}

func (d *namedDriver) Name() string { return d.name }

// pluginProber reports the vendor container plugin it is told to.
type pluginProber struct {
	gpu.FixedProber
	plugin runtime.PluginInfo
	asked  string
}

func (p *pluginProber) DetectRuntimePlugin(_ context.Context, driver string) runtime.PluginInfo {
	p.asked = driver
	return p.plugin
}

// Real cards, unlike gpu.FixedProber's: a synthetic card is injected by no
// vendor plugin, so it is exempt from this question entirely
// (TestRuntimeGPUDoesNotJudgeSyntheticCardsByTheHostsIntegration), and a
// fixture that forgot to say so would pass every case for that reason.
func (p *pluginProber) Synthetic() bool { return false }

// countingPluginProber also counts how often the vendor was asked.
type countingPluginProber struct {
	pluginProber
	calls int
}

func (p *countingPluginProber) DetectRuntimePlugin(ctx context.Context, driver string) runtime.PluginInfo {
	p.calls++
	return p.pluginProber.DetectRuntimePlugin(ctx, driver)
}

func runtimeNode(t *testing.T, vendor, driverName string, plugin runtime.PluginInfo) (*NodeRuntime, *pluginProber) {
	t.Helper()
	prober := &pluginProber{FixedProber: gpu.FixedProber{VendorName: vendor}, plugin: plugin}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), &namedDriver{name: driverName}, t.TempDir(), driverName, prober, nil,
		engine.WithNetworkInterfaceResolver(nil))
	return n, prober
}

// A node whose cards are fine and whose runtime cannot hand them to a
// container: every deployment placed here fails at container start, and
// nothing about the cards would say why. The reason travels as a code —
// the sentence an operator reads is the client's — and the display string
// names only a plugin that is installed: which one a node needs is the
// server's to say (accelerator.RuntimeGPUPlugin).
func TestRuntimeGPUReportsTheMissingPluginAsACode(t *testing.T) {
	n, prober := runtimeNode(t, "nvidia", "docker", runtime.PluginInfo{Name: "NVIDIA Container Toolkit"})
	plugin, blocked := n.RuntimeGPU(context.Background())
	if blocked != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("blocked = %q, want the code for a missing plugin", blocked)
	}
	if plugin != "" {
		t.Fatalf("plugin = %q, want nothing displayed for a plugin that is not installed", plugin)
	}
	if prober.asked != "docker" {
		t.Fatalf("the plugin was looked for under %q", prober.asked)
	}
}

// One detection answers both: the display string and the verdict never
// come from two separate calls to the vendor's tooling, which on a wedged
// node is one of the slow ones.
func TestRuntimeGPUAsksTheVendorOnce(t *testing.T) {
	prober := &countingPluginProber{pluginProber: pluginProber{FixedProber: gpu.FixedProber{VendorName: "nvidia"},
		plugin: runtime.PluginInfo{Name: "NVIDIA Container Toolkit", Version: "1.17.0", Installed: true}}}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), &namedDriver{name: "docker"}, t.TempDir(), "docker", prober, nil,
		engine.WithNetworkInterfaceResolver(nil))
	plugin, blocked := n.RuntimeGPU(context.Background())
	if prober.calls != 1 {
		t.Fatalf("the vendor was asked %d times, want once", prober.calls)
	}
	if plugin != "NVIDIA Container Toolkit 1.17.0" || blocked != "" {
		t.Fatalf("plugin = %q blocked = %q", plugin, blocked)
	}
}

func TestRuntimeGPUBlockedIsSilentWhenNothingIsMissing(t *testing.T) {
	for _, tc := range []struct {
		vendor, driver string
		plugin         runtime.PluginInfo
		why            string
	}{
		{"nvidia", "docker", runtime.PluginInfo{Name: "NVIDIA Container Toolkit", Installed: true}, "the plugin is installed"},
		{"nvidia", "udocker", runtime.PluginInfo{Name: "NVIDIA Container Toolkit"}, "udocker binds the device nodes itself"},
		{"ascend", "nerdctl", runtime.PluginInfo{Name: "Ascend Docker Runtime"}, "nerdctl is given the devices directly"},
		{"ppu", "docker", runtime.PluginInfo{}, "PPU exposes its cards by device passthrough"},
		{"", "docker", runtime.PluginInfo{}, "a node with no accelerator has nothing to expose"},
	} {
		n, _ := runtimeNode(t, tc.vendor, tc.driver, tc.plugin)
		if _, reason := n.RuntimeGPU(context.Background()); reason != "" {
			t.Errorf("%s + %s: reported %q, want silence (%s)", tc.vendor, tc.driver, reason, tc.why)
		}
	}
}

// A vendor that declares no plugin for the runtime cannot be said to be
// missing one.
func TestRuntimeGPUBlockedIsSilentWhenTheVendorNamesNoPlugin(t *testing.T) {
	n, _ := runtimeNode(t, "nvidia", "docker", runtime.PluginInfo{})
	if _, reason := n.RuntimeGPU(context.Background()); reason != "" {
		t.Fatalf("reported %q although the vendor names no plugin", reason)
	}
}

// Without a container runtime at all there is nothing to report about one.
func TestRuntimeGPUBlockedIsSilentWithoutARuntime(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none",
		&gpu.FixedProber{VendorName: "nvidia"}, nil)
	if _, reason := n.RuntimeGPU(context.Background()); reason != "" {
		t.Fatalf("reported %q with no runtime driver", reason)
	}
}

// The daemon's own answer settles NVIDIA + Docker, and a toolkit on the
// agent's PATH does not: installing it also installs its hook, and neither
// says what the running dockerd was configured with. Each report asks
// again, so configuring Docker clears the refusal without an agent restart.
func TestRuntimeGPUTrustsTheDockerDaemonOverTheAgentPATH(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	for _, name := range []string{"nvidia-ctk", "nvidia-container-runtime-hook"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeDocker := func(info string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n[ \"$*\" = 'info --format {{json .}}' ] || exit 42\nprintf '%s\\n' '"+info+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), docker.New(), t.TempDir(), "docker", &gpu.SMIProber{}, nil)

	writeDocker(`{"ServerVersion":"27.5.0","Runtimes":{"runc":{}}}`)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("blocked = %q although the daemon reports no NVIDIA runtime", blocked)
	}
	writeDocker(`{"ServerVersion":"27.5.0","Runtimes":{"runc":{},"nvidia":{}}}`)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("blocked = %q after the daemon registered the runtime", blocked)
	}
}

// The server cordons on this code, and only an operator lifts that. A
// daemon that could not be asked — restarting, or busy past the timeout —
// has said nothing about the node, so the last real answer stands rather
// than a guess: guessing "blocked" cordoned a good node for good on one
// slow `docker info`.
func TestRuntimeGPUKeepsTheLastAnswerWhenTheDaemonCannotBeAsked(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	writeDocker := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n[ \"$*\" = 'info --format {{json .}}' ] || exit 42\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	unreachable := "echo 'Cannot connect to the Docker daemon' >&2; exit 1"
	// The toolkit is installed, so a daemon without NVIDIA support is
	// unconfigured rather than missing its plugin.
	if err := os.WriteFile(filepath.Join(dir, "nvidia-ctk"), []byte("#!/bin/sh\necho 'NVIDIA Container Toolkit CLI version 1.17.3'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	n := newNodeRuntime(slog.New(slog.DiscardHandler), docker.New(), t.TempDir(), "docker", &gpu.SMIProber{}, nil)

	writeDocker(unreachable)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("blocked = %q before the daemon ever answered", blocked)
	}
	writeDocker(`printf '%s\n' '{"ServerVersion":"27.5.0","Runtimes":{"runc":{}}}'`)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("blocked = %q although the daemon reports no NVIDIA runtime", blocked)
	}
	writeDocker(unreachable)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != accelerator.RuntimeGPUBlockedRuntimeUnconfigured {
		t.Fatalf("blocked = %q: an unanswered round dropped the daemon's last answer", blocked)
	}
	writeDocker(`printf '%s\n' '{"ServerVersion":"27.5.0","Runtimes":{"runc":{},"nvidia":{}}}'`)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("blocked = %q after the daemon registered the runtime", blocked)
	}
	writeDocker(unreachable)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("blocked = %q: an unanswered round invented a problem", blocked)
	}
}

// A runtime with no daemon to ask is judged by the plugin, because that is
// all there is: nerdctl consumes the toolkit directly and registers
// nothing. The daemon check must not be borrowed for it.
func TestRuntimeGPUJudgesNerdctlByThePluginItself(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nerdctl.New(), t.TempDir(), "nerdctl", &gpu.SMIProber{}, nil)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != accelerator.RuntimeGPUBlockedPluginMissing {
		t.Fatalf("blocked = %q with no toolkit on a nerdctl node", blocked)
	}
}

// Synthetic cards are injected by no vendor plugin at all, so the host's
// NVIDIA integration says nothing about whether they can be used — and
// asking Docker about it would be a slow call with a misleading answer.
func TestRuntimeGPUDoesNotJudgeSyntheticCardsByTheHostsIntegration(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	n := newNodeRuntime(slog.New(slog.DiscardHandler), docker.New(), t.TempDir(), "docker",
		&gpu.FixedProber{VendorName: "nvidia"}, nil)
	if _, blocked := n.RuntimeGPU(context.Background()); blocked != "" {
		t.Fatalf("synthetic cards need no host NVIDIA integration, blocked=%q", blocked)
	}
}
