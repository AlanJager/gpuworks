package engine

import (
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func (e *Engine) applyDeviceInjection(ws *runtime.WorkloadSpec, indexes []int) error {
	if e.prober == nil || len(indexes) == 0 {
		return nil
	}
	driverName := ""
	if d := e.Driver(); d != nil {
		driverName = d.Name()
	}
	var inj gpu.RuntimeInjection
	if aware, ok := e.prober.(gpu.DriverAwareInjector); ok {
		inj = aware.RuntimeInjectionForDriver(indexes, driverName)
	} else {
		inj = e.prober.RuntimeInjection(indexes)
	}
	if inj.UnsupportedReason != "" {
		return fmt.Errorf("accelerator runtime unavailable: %s", inj.UnsupportedReason)
	}
	for _, m := range inj.Mounts {
		ws.Mounts = append(ws.Mounts, runtime.Mount{Source: m.Source, Target: m.Target, ReadOnly: m.ReadOnly})
	}
	for k, v := range inj.Env {
		// NVIDIA Container Toolkit uses the host device IDs from --gpus to
		// construct a compact container-visible inventory. Reusing the host
		// indexes in CUDA_VISIBLE_DEVICES then hides non-zero selected cards
		// (for example host device 1 is CUDA device 0 inside a one-card
		// container). nerdctl's --gpus goes through the same toolkit hook.
		// Udocker has no such remapping and continues to use the physical
		// indexes supplied by the prober.
		if k == "CUDA_VISIBLE_DEVICES" && (driverName == "docker" || driverName == "nerdctl") && hasDockerOption(inj.DockerArgs, "--gpus") {
			v = compactDeviceIndexes(len(indexes))
		}
		ws.Env[k] = v
	}
	ws.DockerArgs = mergeDeviceDockerArgs(ws.DockerArgs, inj.DockerArgs)
	return nil
}

// mergeDeviceDockerArgs makes the agent's scheduled accelerator selection
// authoritative over image- or user-provided singleton runtime options. In
// particular, an image-level `--gpus all` must not remain alongside the
// scheduler-selected `--gpus '"device=0,1"'`. Repeatable options such as
// --device are intentionally kept.
func mergeDeviceDockerArgs(existing, injected []string) []string {
	result := append([]string(nil), existing...)
	for _, option := range []string{"--gpus", "--runtime"} {
		if hasDockerOption(injected, option) {
			result = removeDockerOption(result, option)
		}
	}
	return append(result, injected...)
}

func hasDockerOption(args []string, option string) bool {
	for _, arg := range args {
		if arg == option || strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func removeDockerOption(args []string, option string) []string {
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if strings.HasPrefix(arg, option+"=") {
			continue
		}
		if arg == option {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		result = append(result, arg)
	}
	return result
}

func compactDeviceIndexes(count int) string {
	indexes := make([]string, count)
	for index := range indexes {
		indexes[index] = strconv.Itoa(index)
	}
	return strings.Join(indexes, ",")
}
