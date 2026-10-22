package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// CheckNVIDIAGPU checks the Docker daemon's GPU integration without pulling an
// image or starting a container. It runs with the minute-level node report, not
// the heartbeat. Toolkit installation alone does not configure dockerd.
func (d *Driver) CheckNVIDIAGPU(ctx context.Context) error {
	out, err := output(ctx, 5*time.Second, "info", "--format", "{{json .}}")
	if err != nil {
		detail := strings.TrimSpace(out)
		if len(detail) > 400 {
			detail = detail[len(detail)-400:]
		}
		return fmt.Errorf("check Docker NVIDIA GPU support: %w: %s", err, detail)
	}
	var info struct {
		Runtimes          map[string]json.RawMessage
		ServerVersion     string
		DiscoveredDevices []struct{ ID string }
	}
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		return fmt.Errorf("decode Docker GPU support: %w", err)
	}
	if raw, ok := info.Runtimes["nvidia"]; ok {
		// Removing the toolkit leaves its registration in daemon.json, and
		// Docker lists the runtime whatever is at its path; every container
		// start then fails. A path that does not resolve settles it, as for
		// the hook below; one that does is the daemon's word.
		var registered struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &registered) == nil && registered.Path != "" {
			if _, err := exec.LookPath(registered.Path); err != nil {
				return fmt.Errorf("Docker registers the nvidia runtime but %s is not installed; install NVIDIA Container Toolkit: %w", registered.Path, runtime.ErrNoGPUSupport)
			}
		}
		return nil
	}
	// A hook installed on the agent's PATH is not evidence that the running
	// Docker daemon has configured NVIDIA support. Only trust daemon facts.
	// Docker 29.2 added CDI-backed --gpus requests. Earlier versions require
	// explicit --device CDI selectors, which our NVIDIA launch does not use.
	var major, minor int
	if _, err := fmt.Sscanf(info.ServerVersion, "%d.%d", &major, &minor); err == nil && (major > 29 || major == 29 && minor >= 2) {
		for _, device := range info.DiscoveredDevices {
			if !strings.HasPrefix(device.ID, "nvidia.com/gpu=") {
				continue
			}
			// The hook's absence, unlike its presence, does settle it:
			// without it Docker hands --gpus to another vendor's CDI driver
			// ("AMD CDI spec not found"), and the CDI spec the toolkit left
			// behind names hooks that are gone with it. Seen on Docker 29.5
			// after the toolkit was removed (2026-10-17).
			if _, err := exec.LookPath(nvidiaHook); err != nil {
				return fmt.Errorf("Docker lists NVIDIA CDI devices but %s is not installed, so --gpus cannot reach them; install NVIDIA Container Toolkit: %w", nvidiaHook, runtime.ErrNoGPUSupport)
			}
			return nil
		}
	}
	return fmt.Errorf("Docker has no NVIDIA GPU support; configure NVIDIA Container Toolkit with nvidia-ctk runtime configure --runtime=docker and restart Docker: %w", runtime.ErrNoGPUSupport)
}

// nvidiaHook is the toolkit binary Docker looks for to serve NVIDIA --gpus
// requests at all.
const nvidiaHook = "nvidia-container-runtime-hook"
