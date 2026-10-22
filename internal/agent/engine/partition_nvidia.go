package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// nvidiaPartitionInjector implements the NVIDIA logical partition backend:
// the embedded HAMi-core libvgpu.so is extracted to the state directory,
// mounted into the container, and activated through LD_PRELOAD plus
// CUDA_DEVICE_* env. vramLimitBytes limits per-GPU memory; smLimitPct
// optionally caps compute.
type nvidiaPartitionInjector struct{}

func (nvidiaPartitionInjector) apply(e *Engine, _ context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if spec.VRAMLimitBytes <= 0 {
		return true, "" // whole-GPU; no injection
	}
	if len(spec.GPUIndexes) == 0 {
		return false, "GPU partitioning requested but no GPU indexes were assigned"
	}
	e.log.Info("applying GPU partitioning", "uuid", spec.UUID, "vram_limit_bytes", spec.VRAMLimitBytes, "sm_limit_pct", spec.SMLimitPct)
	libDir := filepath.Join(e.stateDir, "dgpu")
	libHost := filepath.Join(libDir, "libvgpu.so")
	extract := e.extractLibvgpu
	if extract == nil {
		extract = assets.ExtractLibvgpu
	}
	if _, err := extract(libHost); err != nil {
		return false, "GPU partitioning requested but libvgpu unavailable: " + err.Error()
	}
	const libInContainer = accelerator.DGPULibraryTarget
	cacheHost := filepath.Join(e.stateDir, "dgpu-cache")
	cacheMode := os.FileMode(0o777) | os.ModeSticky
	if err := os.MkdirAll(cacheHost, cacheMode); err != nil {
		return false, "GPU partitioning requested but cache directory is unavailable: " + err.Error()
	}
	// The image may run as a UID that does not exist on the host. A sticky,
	// world-writable directory lets libvgpu create its per-instance cache while
	// preventing one container user from removing another user's file. Chmod is
	// required because MkdirAll preserves the mode of an existing directory.
	if err := os.Chmod(cacheHost, cacheMode); err != nil {
		return false, "GPU partitioning requested but cache permissions cannot be set: " + err.Error()
	}
	// Fresh cache per (re)start avoids stale-limit inheritance.
	cachePath := filepath.Join(cacheHost, spec.UUID+".cache")
	_ = os.Remove(cachePath)
	// Pre-create the file as the agent user. Rootless udocker/PRoot can reject
	// libvgpu's O_CREAT across a bind mount even when the directory is writable;
	// opening an existing 0666 file avoids that runtime-specific edge.
	cacheFile, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return false, "GPU partitioning requested but cache file is unavailable: " + err.Error()
	}
	if err := cacheFile.Chmod(0o666); err != nil {
		_ = cacheFile.Close()
		return false, "GPU partitioning requested but cache permissions cannot be set: " + err.Error()
	}
	if err := cacheFile.Close(); err != nil {
		return false, "GPU partitioning requested but cache file cannot be closed: " + err.Error()
	}

	ws.Mounts = append(ws.Mounts,
		agentruntime.Mount{Source: libHost, Target: libInContainer, ReadOnly: true},
		agentruntime.Mount{Source: cacheHost, Target: accelerator.DGPUCacheTarget},
	)
	for k, v := range buildLibvgpuInjection(libInContainer, spec.UUID, accelerator.DGPUCacheTarget, len(spec.GPUIndexes), spec.VRAMLimitBytes, spec.SMLimitPct) {
		ws.Env[k] = v
	}
	return true, ""
}

// The NVIDIA asset is embedded in the agent binary and reported by the app
// layer at startup; there are no host filesystem bundles to discover.
func (nvidiaPartitionInjector) assetReports(*Engine) []proto.AssetReport { return nil }

// applyLimit rewrites the limit in the instance's HAMi-core shared cache,
// which the interposer consults on every allocation; the cache is rebuilt
// from the spec's environment at the next start, which carries the same
// limit. A limit below what the instance already holds is refused by
// dgpucache and left for a restart.
func (nvidiaPartitionInjector) applyLimit(e *Engine, spec proto.InstanceDesiredState) (bool, error) {
	return dgpucache.SetMemoryLimit(e.DGPUCachePath(spec.UUID), len(spec.GPUIndexes), spec.VRAMLimitBytes)
}

// libvgpu keeps no per-instance host state outside the cache directory, which
// is rebuilt on every start.
func (nvidiaPartitionInjector) cleanupInstance(*Engine, string) error { return nil }

// buildLibvgpuInjection assembles the env a container needs for memory
// limiting. The shared-cache path is unique per instance (changing the limit
// = new path, avoiding the stale-cache inheritance pitfall).
func buildLibvgpuInjection(libvgpuPath, instanceUUID, cacheDir string, gpuCount int, vramLimitBytes int64, smLimitPct int) map[string]string {
	slog.Debug("dGPU injection built", "instance", instanceUUID, "gpu_count", gpuCount, "vram_limit_bytes", vramLimitBytes, "sm_limit_pct", smLimitPct)
	env := map[string]string{
		accelerator.EnvLDPreload: libvgpuPath,
		// Per-instance unique cache path (changing the limit yields a new path,
		// sidestepping stale-cache inheritance; the agent also rm's it pre-start).
		accelerator.DGPUEnvMemoryCache: filepath.Join(cacheDir, instanceUUID+".cache"),
	}
	// libvgpu reads CUDA_DEVICE_MEMORY_LIMIT_<i> per visible device index.
	for i := 0; i < gpuCount; i++ {
		env[accelerator.DGPUEnvMemoryLimitPrefix+strconv.Itoa(i)] = fmt.Sprintf("%d", vramLimitBytes)
	}
	if smLimitPct > 0 && smLimitPct < 100 {
		// HAMi-core switches its limiter on by itself when it creates the
		// shared region; the limit is the only thing it needs.
		env[accelerator.DGPUEnvSMLimit] = strconv.Itoa(smLimitPct)
	}
	return env
}

// DGPUCachePath returns the agent-host libvgpu usage-cache path for an instance.
func (e *Engine) DGPUCachePath(uuid string) string {
	return filepath.Join(e.stateDir, "dgpu-cache", uuid+".cache")
}
