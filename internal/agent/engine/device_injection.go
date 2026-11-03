package engine

import (
	"fmt"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"path/filepath"
	"strings"
)

func (e *Engine) applyDeviceInjection(ws *runtime.WorkloadSpec, indexes []int) error {
	driverName := ""
	if d := e.Driver(); d != nil {
		driverName = d.Name()
	}
	return e.applyDeviceInjectionForDriver(ws, indexes, driverName)
}

func (e *Engine) applyDeviceInjectionForDriver(ws *runtime.WorkloadSpec, indexes []int, driverName string) error {
	if e.prober == nil {
		return nil
	}
	if len(indexes) == 0 {

		return nil
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
	applyRuntimeInjection(ws, inj, indexes, driverName)
	return nil
}

// applyRuntimeInjection applies only a fully validated injection. Unsets and
// additions are built on copies, then committed together so callers never see
// a partially applied device selection.
func applyRuntimeInjection(ws *runtime.WorkloadSpec, inj gpu.RuntimeInjection, indexes []int, driverName string) {
	updated := *ws
	env := make(map[string]string, len(ws.Env)+len(inj.Env))
	for key, value := range ws.Env {
		env[key] = value
	}
	if inj.ManagedSelection != nil {
		for _, key := range inj.ManagedSelection.Env {
			delete(env, key)
		}
	}
	for key, value := range inj.Env {
		env[key] = value
	}
	updated.Env = env

	args := append([]string(nil), ws.DockerArgs...)
	if cleanup := inj.ManagedSelection; cleanup != nil {
		args = removeDockerDevicePaths(args, cleanup.DevicePaths)
		for _, option := range cleanup.DockerOptions {
			args = removeDockerOption(args, option)
		}
	}
	updated.DockerArgs = append(args, inj.DockerArgs...)
	if len(inj.Mounts) > 0 {
		mounts := append([]runtime.Mount(nil), ws.Mounts...)
		for _, mount := range inj.Mounts {
			mounts = append(mounts, runtime.Mount{Source: mount.Source, Target: mount.Target, ReadOnly: mount.ReadOnly})
		}
		updated.Mounts = mounts
	}
	*ws = updated
}

// mergeDeviceDockerArgs makes the agent's scheduled accelerator selection
// authoritative over image- or user-provided singleton runtime options. In
// particular, an image-level --gpus all must not remain alongside a scheduler
// selected --gpus device selector. Repeatable options such as --device are
// removed only when the selected prober declares them managed.

// removeDockerDevicePaths removes only --device mappings whose host source
// names a prober-managed device path. Other devices and malformed options are
// preserved; AMD needs to replace GPU nodes without dropping unrelated device
// access such as /dev/infiniband.
func removeDockerDevicePaths(args, paths []string) []string {
	managed := make(map[string]bool, len(paths))
	for _, path := range paths {
		managed[filepath.Clean(path)] = true
	}
	result := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--device" {
			if index+1 == len(args) {
				result = append(result, arg)
				continue
			}
			value := args[index+1]
			if managedDockerDevice(value, managed) {
				index++
				continue
			}
			result = append(result, arg, value)
			index++
			continue
		}
		if value, ok := strings.CutPrefix(arg, "--device="); ok && managedDockerDevice(value, managed) {
			continue
		}
		result = append(result, arg)
	}
	return result
}

func managedDockerDevice(value string, managed map[string]bool) bool {
	// DRM by-path aliases contain the colon in their PCI BDF, so split-at-first
	// colon parsing does not recover the source path. Match declared full source
	// paths first, then retain the existing parser for ordinary device paths.
	for path := range managed {
		if value == path || strings.HasPrefix(value, path+":") {
			return true
		}
	}
	return managed[filepath.Clean(dockerDeviceSource(value))]
}

func dockerDeviceSource(value string) string {
	return strings.SplitN(value, ":", 2)[0]
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
