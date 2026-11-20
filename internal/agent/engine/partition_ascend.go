package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

// ascendPartitionInjector implements the Ascend logical partition backend:
// host-side libvruntime.so plus per-instance ld.so.preload / npu_info.config
// files are mounted into the container, and a virtual NPU id is allocated
// from the node-wide ledger so colocated instances get stable identities.
type ascendPartitionInjector struct{}

func (a ascendPartitionInjector) apply(e *Engine, ctx context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	return a.applyWithUsageMount(e, ctx, ws, spec, canMountAscendUsageLedger(os.Geteuid(), e.Driver()))
}

func (ascendPartitionInjector) applyWithUsageMount(e *Engine, ctx context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState, mountUsage bool) (bool, string) {
	if len(spec.GPUIndexes) != 1 {
		return false, "Ascend logical slicing currently requires exactly one GPU per instance"
	}
	e.seedImageLibraryPath(ctx, ws)
	e.partitionMu.Lock()
	defer e.partitionMu.Unlock()
	physicalID := spec.GPUIndexes[0]
	if host, ok := e.prober.(interface {
		EnsureDeviceShare(context.Context, int) (bool, error)
	}); ok {
		changed, err := host.EnsureDeviceShare(ctx, physicalID)
		if err != nil {
			return false, err.Error()
		}
		note := fmt.Sprintf("Ascend device-share enabled: physical card %d", physicalID)
		if changed {
			note = fmt.Sprintf("Ascend device-share auto-enabled: physical card %d", physicalID)
		}
		ws.PreparationNotes = append(ws.PreparationNotes, note)
	}
	backend, _ := accelerator.Lookup(accelerator.BackendAscendLogical)
	asset, err := e.scanPartitionAssets(backend, "ascend", exactAscendAssetDir)
	if err != nil {
		return false, "Ascend partition asset unavailable: " + err.Error()
	}
	dir := filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(spec.UUID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, "Ascend partition state directory: " + err.Error()
	}
	libTarget := accelerator.AscendLibraryTarget
	preloadHost := filepath.Join(dir, "ld.so.preload")
	if err := writePrivateFile(preloadHost, []byte("/usr/local/dcmi/libdcmi.so\n/usr/local/Ascend/driver/lib64/driver/libdcmi.so\n"+libTarget+"\n"), 0o644); err != nil {
		return false, "write Ascend preload config: " + err.Error()
	}
	quotaMiB := (spec.VRAMLimitBytes + (1 << 20) - 1) / (1 << 20)
	aicore := spec.SMLimitPct
	if aicore <= 0 {
		aicore = 100
	}
	virtualID, err := e.lowestFreeVirtualNPUID(dir, physicalID)
	if err != nil {
		return false, "allocate Ascend virtual NPU id: " + err.Error()
	}
	shmID := "card-" + strconv.Itoa(physicalID)
	if host, ok := e.prober.(interface {
		PartitionHardwareID(context.Context, int) string
	}); ok {
		if stable := sanitizeHardwareID(host.PartitionHardwareID(ctx, physicalID)); stable != "" {
			shmID = stable
		}
	}
	configHost := filepath.Join(dir, "npu_info.config")
	config := fmt.Sprintf("physical-npu-id=%d\nvirtual-npu-id=%d\naicore-quota=%d\nmemory-quota=%d\nshm-id=%s\nscheduling-policy=2\n", physicalID, virtualID, aicore, quotaMiB, shmID)
	if err := writePrivateFile(configHost, []byte(config), 0o644); err != nil {
		return false, "write Ascend slice config: " + err.Error()
	}
	// vcann-rt keeps what the slice is charged in a POSIX shared memory
	// object named after the slice, which a container with an IPC namespace
	// of its own keeps to itself. Backing that name with a file of the
	// agent's is what lets the collector read what the slice holds.
	if mountUsage {
		ledgerHost := e.usageLedgerPath(spec.UUID)
		if err := freshUsageLedger(ledgerHost); err != nil {
			return false, "Ascend partition ledger: " + err.Error()
		}
		ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: ledgerHost, Target: ascendLedgerTarget(shmID, virtualID)})
	}
	ws.Mounts = append(ws.Mounts,
		agentruntime.Mount{Source: asset.Files["libvruntime.so"], Target: libTarget, ReadOnly: true},
		agentruntime.Mount{Source: preloadHost, Target: accelerator.AscendPreloadTarget, ReadOnly: true},
		agentruntime.Mount{Source: configHost, Target: accelerator.AscendConfigTarget, ReadOnly: true},
	)
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}
	ws.Env[accelerator.AscendEnvDSMIHook] = "1"
	driverLibraryPath := accelerator.AscendDriverLibraryPath
	if existing := strings.TrimSpace(ws.Env[accelerator.EnvLDLibraryPath]); existing != "" {
		ws.Env[accelerator.EnvLDLibraryPath] = driverLibraryPath + ":" + existing
	} else {
		ws.Env[accelerator.EnvLDLibraryPath] = driverLibraryPath
	}
	return true, ""
}

// Linux may deny shm_open(O_CREAT) in sticky /dev/shm when the bind source
// is owned by another UID, even when the file already exists and is writable.
// A root agent can supply a file owned by the directory owner. PRoot shares
// the host's /dev/shm directly, so it needs no bind.
func canMountAscendUsageLedger(agentUID int, driver agentruntime.Driver) bool {
	return agentUID == 0 && (driver == nil || driver.Name() != "udocker")
}

// ascendLedgerTarget is where vcann-rt's shm_open finds its memory ledger
// inside the container: "/enpu-vmem-<shm-id>-<virtual-npu-id>"
// (src/memory_ledger.c in the patched source the asset is built from).
func ascendLedgerTarget(shmID string, virtualID int) string {
	return fmt.Sprintf("/dev/shm/enpu-vmem-%s-%d", shmID, virtualID)
}

// usage reads the slice's memory ledger. An Ascend slice is one card.
func (ascendPartitionInjector) usage(e *Engine, spec proto.InstanceDesiredState) ([]dgpucache.DeviceUsage, error) {
	device, err := dgpucache.ReadAscendLedger(e.usageLedgerPath(spec.UUID))
	if errors.Is(err, os.ErrNotExist) {
		// PRoot and OCI workloads with host IPC can use the ledger the shim
		// creates in /dev/shm. This also covers slices already running when
		// the agent is upgraded.
		data, readErr := os.ReadFile(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(spec.UUID), "npu_info.config"))
		if readErr != nil {
			return nil, readErr
		}
		config := parsePartitionConfig(string(data))
		shmID := sanitizeHardwareID(config["shm-id"])
		virtualID, idErr := strconv.Atoi(config["virtual-npu-id"])
		if shmID == "" || idErr != nil || virtualID < 0 {
			return nil, fmt.Errorf("Ascend partition config has no valid usage ledger name")
		}
		device, err = dgpucache.ReadAscendLedger(e.hostRoot + ascendLedgerTarget(shmID, virtualID))
	}
	if err != nil {
		return nil, err
	}
	return []dgpucache.DeviceUsage{device}, nil
}

func (ascendPartitionInjector) assetReports(e *Engine) []proto.AssetReport {
	return scanAssetReports(e, accelerator.BackendAscendLogical, "ascend", exactAscendAssetDir)
}

// The quota reaches the backend through its start-time environment and
// config only.
func (ascendPartitionInjector) applyLimit(*Engine, proto.InstanceDesiredState) (bool, error) {
	return false, errLiveLimitUnsupported
}

func (ascendPartitionInjector) cleanupInstance(e *Engine, uuid string) error {
	return os.RemoveAll(filepath.Join(e.stateDir, partitionStateRoot, "instances", sanitizeID(uuid)))
}

// exactAscendAssetDir pins the Ascend bundle to the arch directory; legacy
// version-suffixed directories under it must not be selected, because their
// contents are keyed to a CANN version the current injection does not use.
func exactAscendAssetDir(root, aliasDir, dir string) bool {
	return filepath.Clean(dir) == filepath.Join(root, aliasDir, "linux-"+stdruntime.GOARCH)
}

// seedImageLibraryPath preserves an image's own runtime/toolkit search path
// before Ascend partition injection prepends host-driver directories. Passing
// -e LD_LIBRARY_PATH to docker replaces (rather than extends) Config.Env; not
// hydrating it first can make an otherwise valid CANN image lose libhccl.so.
// Explicit deployment env always wins, and runtimes without inspection retain
// the minimal driver-only path used by the embedded probe image.
func (e *Engine) seedImageLibraryPath(ctx context.Context, ws *agentruntime.WorkloadSpec) {
	driver := e.Driver()
	if driver == nil || strings.TrimSpace(ws.Env[accelerator.EnvLDLibraryPath]) != "" {
		return
	}
	inspector, ok := driver.(agentruntime.ImageInspector)
	if !ok {
		return
	}
	inspection, err := inspector.InspectImage(ctx, ws.Image)
	if err != nil {
		return
	}
	if libraryPath := strings.TrimSpace(inspection.Env[accelerator.EnvLDLibraryPath]); libraryPath != "" {
		ws.Env[accelerator.EnvLDLibraryPath] = libraryPath
	}
}

// lowestFreeVirtualNPUID allocates a stable per-card virtual NPU id. An
// instance reusing its own directory keeps its id; a restarted node reuses
// the lowest hole once the owning instance directory is gone.
func (e *Engine) lowestFreeVirtualNPUID(ownDir string, physicalID int) (int, error) {
	instancesRoot := filepath.Join(e.stateDir, partitionStateRoot, "instances")
	used := map[int]bool{}
	ownID := -1
	entries, err := os.ReadDir(instancesRoot)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		configPath := filepath.Join(instancesRoot, entry.Name(), "npu_info.config")
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			continue
		}
		fields := parsePartitionConfig(string(data))
		card, cardErr := strconv.Atoi(fields["physical-npu-id"])
		vid, vidErr := strconv.Atoi(fields["virtual-npu-id"])
		if cardErr != nil || vidErr != nil || card != physicalID || vid < 0 {
			continue
		}
		if filepath.Clean(filepath.Dir(configPath)) == filepath.Clean(ownDir) {
			ownID = vid
			continue
		}
		used[vid] = true
	}
	if ownID >= 0 && !used[ownID] {
		return ownID, nil
	}
	for id := 0; ; id++ {
		if !used[id] {
			return id, nil
		}
	}
}

func parsePartitionConfig(raw string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields
}

func sanitizeHardwareID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out.WriteRune(r)
		} else if out.Len() > 0 {
			out.WriteByte('-')
		}
	}
	return strings.Trim(out.String(), "-")
}
