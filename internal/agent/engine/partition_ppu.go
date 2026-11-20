package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// ppuPartitionInjector implements the PPU logical partition backend: the
// ppu-slicing-shim quota/hook libraries are mounted into the container and
// activated through LD_PRELOAD and HGGC_DEVICE_* env. A file in the instance's
// partition state directory backs its usage ledger, independently of the
// container's IPC namespace.
//
// On the kube driver the ledger is a file in the host's /dev/shm instead,
// pre-created through e.hostRoot and mounted into the pod: see
// createHostShmLedger in partition.go (shared with partition_amd.go).
type ppuPartitionInjector struct{}

func (ppuPartitionInjector) apply(e *Engine, _ context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if len(spec.GPUIndexes) == 0 {
		return false, "PPU logical slicing requires at least one selected device"
	}
	backend, _ := accelerator.Lookup(accelerator.BackendPPULogical)
	asset, err := e.scanPartitionAssets(backend, "ppu", nil)
	if err != nil {
		return false, "PPU partition asset unavailable: " + err.Error()
	}
	if err := os.MkdirAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(spec.UUID)), 0o755); err != nil {
		return false, "PPU partition state directory: " + err.Error()
	}
	quotaTarget, hgmlTarget := accelerator.PPUQuotaLibraryTarget, accelerator.PPUHookLibraryTarget
	ws.Mounts = append(ws.Mounts,
		agentruntime.Mount{Source: asset.Files["hggc_quota.so"], Target: quotaTarget, ReadOnly: true},
		agentruntime.Mount{Source: asset.Files["hgml_dlsym_hook.so"], Target: hgmlTarget, ReadOnly: true},
	)
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}
	ws.Env[accelerator.EnvLDPreload] = hgmlTarget + ":" + quotaTarget
	ws.Env[accelerator.EnvNVIDIAVisibleDevices] = "void"
	smLimit := spec.SMLimitPct
	if smLimit <= 0 {
		smLimit = 100
	}
	for i := range spec.GPUIndexes {
		ordinal := strconv.Itoa(i)
		ws.Env[accelerator.PPUEnvMemoryLimitPrefix+ordinal] = strconv.FormatInt((spec.VRAMLimitBytes+(1<<20)-1)/(1<<20), 10)
		ws.Env[accelerator.PPUEnvSMLimitPrefix+ordinal] = strconv.Itoa(smLimit)
	}
	ledger := ppuLedgerPath(spec.UUID)
	if e.hostRoot != "" {
		if err := createHostShmLedger(e.hostRoot, ledger); err != nil {
			return false, "PPU partition ledger: " + err.Error()
		}
		ws.Env[accelerator.PPUEnvLedgerPath] = ledger
		ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: ledger, Target: ledger})
		return true, ""
	}
	// The ledger is the slice's usage surface as well as its accounting, and
	// a container with an IPC namespace of its own keeps /dev/shm to itself.
	// Backing the path with a file of the agent's is what lets the collector
	// read what the slice holds whatever the container's IPC mode.
	source := e.usageLedgerPath(spec.UUID)
	if err := freshUsageLedger(source); err != nil {
		return false, "PPU partition ledger: " + err.Error()
	}
	// The shim accepts a file outside /dev/shm, where Linux permits open
	// with O_CREAT even if the container user did not create the file.
	target := filepath.Join(filepath.Dir(quotaTarget), "usage-ledger")
	ws.Env[accelerator.PPUEnvLedgerPath] = target
	ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: source, Target: target})
	return true, ""
}

func (ppuPartitionInjector) assetReports(e *Engine) []proto.AssetReport {
	return scanAssetReports(e, accelerator.BackendPPULogical, "ppu", nil)
}

// The quota reaches the backend through its start-time environment and
// config only.
func (ppuPartitionInjector) applyLimit(*Engine, proto.InstanceDesiredState) (bool, error) {
	return false, errLiveLimitUnsupported
}

func (ppuPartitionInjector) cleanupInstance(e *Engine, uuid string) error {
	err := os.RemoveAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(uuid)))
	// Also remove the host /dev/shm ledger still used by kube and by
	// workloads that started with host IPC before Docker bound the ledger
	// directly. e.hostRoot points to the host filesystem for the kube agent.
	if removeErr := os.Remove(e.hostRoot + ppuLedgerPath(uuid)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}
	return err
}

// usage reads the shim's usage region: the agent's own file behind the
// ledger path, or the host's /dev/shm for the kube driver and for an instance
// started with --ipc=host before the agent backed the path itself.
func (ppuPartitionInjector) usage(e *Engine, spec proto.InstanceDesiredState) ([]dgpucache.DeviceUsage, error) {
	if e.hostRoot == "" {
		devices, err := dgpucache.ReadPPULedger(e.usageLedgerPath(spec.UUID), len(spec.GPUIndexes))
		if !errors.Is(err, os.ErrNotExist) {
			return devices, err
		}
	}
	return dgpucache.ReadPPULedger(e.hostRoot+ppuLedgerPath(spec.UUID), len(spec.GPUIndexes))
}

func ppuLedgerPath(uuid string) string {
	return filepath.Join("/dev/shm", "gpuworks-ppu-"+sanitizeID(uuid))
}
