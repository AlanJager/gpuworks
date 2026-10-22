package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestNVIDIAGPUAccessRejectsUnconfiguredDaemon(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	installFakeDocker(t, `test "$*" = 'info --format {{json .}}' || exit 42
printf '%s\n' '{"ServerVersion":"27.5.0","Runtimes":{"runc":{"path":"runc"}}}'`)
	// Installing the toolkit also installs its hook, but neither proves that
	// the running Docker daemon has configured NVIDIA GPU support.
	for _, name := range []string{"nvidia-ctk", "nvidia-container-runtime-hook"} {
		tool := filepath.Join(strings.Split(os.Getenv("PATH"), ":")[0], name)
		if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := New().CheckNVIDIAGPU(context.Background()); err == nil || !strings.Contains(err.Error(), "NVIDIA") {
		t.Fatalf("unconfigured Docker GPU support = %v, want actionable NVIDIA error", err)
	}
}

func TestNVIDIAGPUAccessPropagatesDaemonFailureAndCancellation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"daemon unreachable", "echo 'Cannot connect to the Docker daemon' >&2; exit 1", "Cannot connect"},
		{"malformed response", "echo broken", "decode Docker"},
		{"empty response", "exit 0", "decode Docker"},
		{"older CDI requires different launch flags", `printf '%s\n' '{"ServerVersion":"28.5.0","Runtimes":{"runc":{}},"DiscoveredDevices":[{"Source":"cdi","ID":"nvidia.com/gpu=0"}]}'`, "NVIDIA"},
		{"unrelated CDI device", `printf '%s\n' '{"ServerVersion":"29.2.0","Runtimes":{"runc":{}},"DiscoveredDevices":[{"Source":"cdi","ID":"vendor.com/device=0"}]}'`, "NVIDIA"},
		// The toolkit was removed and left its CDI spec behind: Docker 29.5
		// still lists the devices, and --gpus fails with "AMD CDI spec not found".
		// The toolkit was removed and left its runtime registered in
		// daemon.json: Docker lists it, and every container start fails.
		{"registered runtime whose binary is gone", `printf '%s\n' '{"ServerVersion":"29.5.1","Runtimes":{"runc":{},"nvidia":{"path":"nvidia-container-runtime"}}}'`, "nvidia-container-runtime is not installed"},
		{"CDI devices without the toolkit's hook", `printf '%s\n' '{"ServerVersion":"29.5.1","Runtimes":{"runc":{}},"DiscoveredDevices":[{"Source":"cdi","ID":"nvidia.com/gpu=0"},{"Source":"cdi","ID":"nvidia.com/gpu=all"}]}'`, "nvidia-container-runtime-hook is not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			installFakeDocker(t, "[ \"$*\" = 'info --format {{json .}}' ] || exit 42\n"+tc.body)
			err := New().CheckNVIDIAGPU(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GPU access failure=%v, want %q", err, tc.want)
			}
			if (strings.Contains(tc.name, "CDI") || strings.Contains(tc.name, "gone")) && !errors.Is(err, runtime.ErrNoGPUSupport) {
				t.Fatalf("GPU access failure=%v is not a definite no", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	installFakeDocker(t, "exit 0")
	if err := New().CheckNVIDIAGPU(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled GPU check=%v", err)
	}
}

func TestNVIDIAGPUAccessAcceptsSupportedInjectionPaths(t *testing.T) {
	for _, tc := range []struct {
		name, info string
	}{
		// The runtime the daemon registered is installed where it says; the
		// toolkit's CLI (nvidia-ctk) need not be on the agent's PATH.
		{"registered runtime without toolkit in agent PATH", `{"ServerVersion":"27.5.0","Runtimes":{"nvidia":{"path":"RUNTIME_PATH"}}}`},
		{"Docker GPU request backed by CDI", `{"ServerVersion":"29.2.0","Runtimes":{"runc":{"path":"runc"}},"DiscoveredDevices":[{"Source":"cdi","ID":"nvidia.com/gpu=0"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			runtimeBinary := filepath.Join(t.TempDir(), "nvidia-container-runtime")
			if err := os.WriteFile(runtimeBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			info := strings.ReplaceAll(tc.info, "RUNTIME_PATH", runtimeBinary)
			installFakeDocker(t, "test \"$*\" = 'info --format {{json .}}' || exit 42\nprintf '%s\\n' '"+info+"'")
			// Docker serves CDI-backed --gpus through the toolkit's hook.
			hook := filepath.Join(strings.Split(os.Getenv("PATH"), ":")[0], "nvidia-container-runtime-hook")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := New().CheckNVIDIAGPU(context.Background()); err != nil {
				t.Fatalf("supported injection rejected: %v", err)
			}
		})
	}
}
