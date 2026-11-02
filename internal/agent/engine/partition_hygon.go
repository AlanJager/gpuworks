package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	hygonHelperPath     = "/opt/gpuworks/bin/gpuworks-hygon-vdev-helper"
	hygonVdevLedgerName = "vdev-id"
	// Shared filesystem protocol with the optional native helper.
	hygonVdevCreatePendingName = "vdev-create-pending"
	hygonVirtualLock           = "/tmp/.dmi_virtual.lock"
)

// hygonVdevHelper is implemented by an optional native helper. The static
// agent never links the vendor's CGO library or destroys physical-card state.
type hygonVdevHelper interface {
	Create(context.Context, int, int, int, string, string) (int, error)
	Inspect(context.Context, hygonOwnedVdev) (hygonVdevInfo, bool, error)
	Release(context.Context, hygonOwnedVdev, string) error
	Prune(context.Context, hygonOwnedVdev, string) error
}

// The driver can reuse a numeric ID after a reset. Persist the expected
// device identity so recovery never treats an arbitrary occupant as ours.
type hygonVdevInfo struct {
	GPUIndex  int    `json:"gpu_index"`
	CUCount   int    `json:"cu_count"`
	MemoryMiB int    `json:"memory_mib"`
	PCIeBDF   string `json:"pci_bdf"`
}

type hygonOwnedVdev struct {
	ID         int    `json:"vdev_id"`
	OwnerToken string `json:"owner_token"`
	hygonVdevInfo
}

func (owned hygonOwnedVdev) matches(info hygonVdevInfo) bool {
	return owned.GPUIndex == info.GPUIndex && owned.CUCount == info.CUCount && owned.MemoryMiB == info.MemoryMiB && strings.EqualFold(owned.PCIeBDF, info.PCIeBDF)
}

type hygonPartitionInjector struct {
	vdevDir    string // empty uses /etc/vdev
	dtkDir     string // empty uses /opt/dtk
	hyhalDir   string // empty uses /opt/hyhal
	helperPath string // empty uses the optional installed helper
	lockPath   string // empty uses the host HYHAL virtual-device lock
	helper     hygonVdevHelper
}

func (h hygonPartitionInjector) paths() (string, string, string, string) {
	vdev, dtk, hyhal, helper := h.vdevDir, h.dtkDir, h.hyhalDir, h.helperPath
	if vdev == "" {
		vdev = "/etc/vdev"
	}
	if dtk == "" {
		dtk = "/opt/dtk"
	}
	if hyhal == "" {
		hyhal = "/opt/hyhal"
	}
	if helper == "" {
		helper = hygonHelperPath
	}
	return vdev, dtk, hyhal, helper
}

func (h hygonPartitionInjector) vdevHelper() hygonVdevHelper {
	if h.helper != nil {
		return h.helper
	}
	_, _, _, path := h.paths()
	return hygonCLIHelper{path: path}
}

func hygonInstanceDir(e *Engine, uuid string) string {
	// Other partition injectors remove dgpu-runtime/instances/<uuid> during
	// teardown. Keep the owned native vdev ledger outside that shared layout.
	return filepath.Join(e.stateDir, partitionStateRoot, "hygon-instances", sanitizeID(uuid))
}

func (h hygonPartitionInjector) apply(e *Engine, ctx context.Context, ws *agentruntime.WorkloadSpec, spec proto.InstanceDesiredState) (bool, string) {
	if e.driver == nil || e.driver.Name() != "docker" {
		return false, "Hygon dGPU requires the Docker runtime"
	}
	if e.prober == nil || accelerator.NormalizeVendor(e.prober.Vendor()) != accelerator.VendorHygon || len(spec.GPUIndexes) != 1 {
		return false, "Hygon dGPU requires exactly one supported Hygon GPU per instance"
	}
	if spec.SMLimitPct != 0 {
		return false, "Hygon compute quota is not supported by the deployment API"
	}
	gpus, err := e.prober.Probe(ctx)
	if err != nil {
		return false, "Hygon inventory: " + err.Error()
	}
	var selected *proto.GPUInfo
	for i := range gpus {
		if gpus[i].Index == spec.GPUIndexes[0] {
			selected = &gpus[i]
			break
		}
	}
	backend, _ := accelerator.Lookup(accelerator.BackendHygonLogical)
	if selected == nil || !backend.Matches(selected.Vendor, selected.ArchID) || selected.PCIeBDF == "" {
		return false, "selected GPU is not a supported Hygon Z100 or K100AI with a PCI address"
	}
	if spec.VRAMLimitBytes <= 0 {
		return false, "Hygon vdev memory quota must be positive and below physical VRAM"
	}
	quota := (spec.VRAMLimitBytes-1)/(1<<20) + 1
	physicalMiB := selected.MemoryTotalBytes / (1 << 20)
	if quota >= physicalMiB {
		return false, "Hygon vdev memory quota must be positive and below physical VRAM"
	}
	physical, ok := e.prober.(interface {
		ComputeUnits(context.Context, int) (int, error)
	})
	if !ok {
		return false, "Hygon physical compute-unit query is unavailable"
	}
	// Fresh vendor queries share the native allocation boundary. Concurrent
	// starts must not accumulate CLI processes waiting on the same driver.
	e.partitionMu.Lock()
	defer e.partitionMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, "Hygon physical compute units: " + err.Error()
	}
	totalCUs, err := physical.ComputeUnits(ctx, selected.Index)
	if err != nil {
		return false, "Hygon physical compute units: " + err.Error()
	}
	if totalCUs <= 0 {
		return false, "Hygon physical compute-unit count must be positive"
	}
	// The current deployment API has no compute-share input. Reserve CUs in
	// proportion to memory so multiple slices can coexist without claiming an
	// independently configurable compute quota.
	// Use full-width arithmetic so even an oversized CLI count cannot wrap.
	hi, lo := bits.Mul64(uint64(quota), uint64(totalCUs))
	quotient, remainder := bits.Div64(hi, lo, uint64(physicalMiB))
	cuCount := int(quotient)
	if remainder != 0 {
		cuCount++
	}
	vdevDir, dtkDir, hyhalDir, helperPath := h.paths()
	for _, path := range []string{
		filepath.Join(dtkDir, "lib", "libamdhip64.so"),
		filepath.Join(dtkDir, "lib", "librocm_smi64.so"),
		filepath.Join(hyhalDir, "lib", "libhydmi.so"),
		filepath.Join(hyhalDir, "lib", "libhydmi_mig.so"),
		helperPath,
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || path == helperPath && info.Mode().Perm()&0o111 == 0 {
			return false, "Hygon runtime asset unavailable: " + path
		}
	}
	instanceDir := hygonInstanceDir(e, spec.UUID)
	if err := checkHygonCreatePending(instanceDir); err != nil {
		return false, err.Error()
	}
	helper := h.vdevHelper()
	owned, found, err := readHygonOwnedVdev(instanceDir)
	if err != nil {
		return false, "read Hygon vdev ownership: " + err.Error()
	}
	if found {
		info, exists, inspectErr := helper.Inspect(ctx, owned)
		if inspectErr != nil {
			return false, "inspect owned Hygon vdev: " + inspectErr.Error()
		}
		if exists && !owned.matches(info) {
			return false, "owned Hygon vdev ID now belongs to a different device"
		}
		if !exists {
			if err := helper.Prune(ctx, owned, filepath.Join(instanceDir, fmt.Sprintf("vdev%d.conf", owned.ID))); err != nil {
				return false, "clear stale Hygon vdev configuration: " + err.Error()
			}
			if err := os.RemoveAll(instanceDir); err != nil {
				return false, "clear stale Hygon vdev state: " + err.Error()
			}
			found = false
		}
	}
	if !found {
		// Check the state directory before creating a native resource. A failed
		// ownership write is still possible, and the rollback error is reported.
		if err := os.MkdirAll(instanceDir, 0o755); err != nil {
			return false, "create Hygon vdev state: " + err.Error()
		}
		entries, err := os.ReadDir(instanceDir)
		if err != nil {
			return false, "inspect Hygon vdev state: " + err.Error()
		}
		if len(entries) != 0 {
			return false, "unclaimed Hygon vdev state requires manual recovery before creating another device"
		}
		ledgerPath := filepath.Join(instanceDir, hygonVdevLedgerName)
		id, createErr := helper.Create(ctx, spec.GPUIndexes[0], cuCount, int(quota), ledgerPath, selected.PCIeBDF)
		if createErr != nil {
			return false, "create Hygon vdev: " + createErr.Error()
		}
		if id < 0 {
			return false, "Hygon vdev helper returned a negative id"
		}
		owned, found, err = readHygonOwnedVdev(instanceDir)
		if err != nil || !found || owned.ID != id || !owned.matches(hygonVdevInfo{GPUIndex: spec.GPUIndexes[0], CUCount: cuCount, MemoryMiB: int(quota), PCIeBDF: selected.PCIeBDF}) {
			return false, fmt.Sprintf("Hygon helper did not persist matching vdev ownership for id %d: %v", id, err)
		}
	}
	if owned.GPUIndex != spec.GPUIndexes[0] || owned.CUCount != cuCount || owned.MemoryMiB != int(quota) || !strings.EqualFold(owned.PCIeBDF, selected.PCIeBDF) {
		return false, "existing Hygon vdev ownership does not match requested GPU or quota"
	}
	hostConfig := filepath.Join(vdevDir, fmt.Sprintf("vdev%d.conf", owned.ID))
	data, err := os.ReadFile(hostConfig)
	if err != nil {
		return false, "read Hygon vdev configuration: " + err.Error()
	}
	fields := parseHygonVdevConfig(string(data))
	if !strings.EqualFold(fields["pcibusid"], selected.PCIeBDF) || fields["vdev_id"] != strconv.Itoa(owned.ID) || fields["mem"] != fmt.Sprintf("%d MiB", quota) || fields["cu_count"] != strconv.Itoa(cuCount) {
		return false, "Hygon helper created a vdev with mismatched PCI, ID, memory or compute units"
	}
	if err := verifyHygonConfigSnapshot(instanceDir, hostConfig, data); err != nil {
		return false, "Hygon vdev configuration changed since ownership was recorded: " + err.Error()
	}
	lockPath := h.lockPath
	if lockPath == "" {
		lockPath = hygonVirtualLock
	}
	if info, err := os.Stat(lockPath); err != nil || !info.Mode().IsRegular() {
		return false, "Hygon virtual-device lock unavailable: " + lockPath
	}
	e.seedImageLibraryPath(ctx, ws)
	snapshot := filepath.Join(instanceDir, filepath.Base(hostConfig))
	// HYHAL reads both paths and serializes access through the host lock.
	// Both filenames must match the verified native ID. Mount exactly one
	// Docker config: a fixed-ID alias can prevent HIP initialization.
	// Keep the image's /opt/dtk: replacing its toolkit with an older host DTK
	// breaks images whose PyTorch requires newer shared-library versions.
	ws.Mounts = append(ws.Mounts,
		agentruntime.Mount{Source: snapshot, Target: filepath.Join("/etc/vdev", filepath.Base(hostConfig)), ReadOnly: true},
		agentruntime.Mount{Source: snapshot, Target: filepath.Join("/etc/vdev/docker", filepath.Base(hostConfig)), ReadOnly: true},
		agentruntime.Mount{Source: lockPath, Target: hygonVirtualLock},
		agentruntime.Mount{Source: dtkDir, Target: "/opt/hygondriver", ReadOnly: true},
		agentruntime.Mount{Source: hyhalDir, Target: "/opt/hyhal", ReadOnly: true},
	)
	if ws.Image == assets.PartitionProbeImage {
		// The embedded probe image has no DTK of its own.
		ws.Mounts = append(ws.Mounts, agentruntime.Mount{Source: dtkDir, Target: "/opt/dtk", ReadOnly: true})
		// HYHAL opens its log directory during HIP initialization. Keep probe
		// logs ephemeral and writable when the container runs as a non-root UID.
		ws.DockerArgs = append(ws.DockerArgs, "--tmpfs", "/var/log/hylog:rw,nosuid,nodev,mode=1777")
	}
	if ws.Env == nil {
		ws.Env = map[string]string{}
	}
	paths := "/opt/dtk/lib:/opt/dtk/lib64:/opt/hyhal/lib"
	if existing := strings.TrimSpace(ws.Env[accelerator.EnvLDLibraryPath]); existing != "" {
		paths += ":" + existing
	}
	ws.Env[accelerator.EnvLDLibraryPath] = paths
	return true, ""
}

func (h hygonPartitionInjector) assetReports(*Engine) []proto.AssetReport {
	_, dtk, hyhal, helper := h.paths()
	files := map[string]string{
		"libamdhip64.so":             filepath.Join(dtk, "lib", "libamdhip64.so"),
		"libhydmi.so":                filepath.Join(hyhal, "lib", "libhydmi.so"),
		"libhydmi_mig.so":            filepath.Join(hyhal, "lib", "libhydmi_mig.so"),
		"librocm_smi64.so":           filepath.Join(dtk, "lib", "librocm_smi64.so"),
		"gpuworks-hygon-vdev-helper": helper,
	}
	for name, path := range files {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || name == "gpuworks-hygon-vdev-helper" && info.Mode().Perm()&0o111 == 0 {
			return nil
		}
	}
	return []proto.AssetReport{assetReport(accelerator.BackendHygonLogical, partitionAsset{Root: "/opt", Files: files})}
}

// Native vdev quotas are fixed for the lifetime of the created device.
func (hygonPartitionInjector) applyLimit(*Engine, proto.InstanceDesiredState) (bool, error) {
	return false, errLiveLimitUnsupported
}

func (h hygonPartitionInjector) cleanupInstance(e *Engine, uuid string) error {
	e.partitionMu.Lock()
	defer e.partitionMu.Unlock()
	dir := hygonInstanceDir(e, uuid)
	if err := checkHygonCreatePending(dir); err != nil {
		return err
	}
	owned, found, err := readHygonOwnedVdev(dir)
	if err != nil {
		return err
	}
	if !found {
		entries, err := os.ReadDir(dir)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect unclaimed Hygon vdev state before cleanup: %w", err)
		}
		if len(entries) != 0 {
			return errors.New("unclaimed Hygon vdev state requires manual recovery before cleanup")
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	helper := h.vdevHelper()
	info, exists, err := helper.Inspect(ctx, owned)
	if err != nil {
		return fmt.Errorf("inspect owned Hygon vdev before cleanup: %w", err)
	}
	if !exists {
		if err := helper.Prune(ctx, owned, filepath.Join(dir, fmt.Sprintf("vdev%d.conf", owned.ID))); err != nil {
			return err
		}
		return os.RemoveAll(dir)
	}
	if !owned.matches(info) {
		return fmt.Errorf("owned Hygon vdev ID now belongs to a different device")
	}
	vdevDir, _, _, _ := h.paths()
	hostConfig := filepath.Join(vdevDir, fmt.Sprintf("vdev%d.conf", owned.ID))
	if err := verifyHygonConfigSnapshot(dir, hostConfig, nil); err != nil {
		return fmt.Errorf("Hygon vdev configuration changed since ownership was recorded: %w", err)
	}
	// The caller confirms that the container is gone before invoking this.
	snapshot := filepath.Join(dir, fmt.Sprintf("vdev%d.conf", owned.ID))
	if releaseErr := helper.Release(ctx, owned, snapshot); releaseErr != nil {
		// Some HYHAL versions report an error from StopVDevice even though the
		// following single-device destroy succeeds. Confirm absence and prune
		// only our matching config before treating this as complete.
		if _, exists, inspectErr := helper.Inspect(ctx, owned); inspectErr != nil || exists {
			return releaseErr
		}
		if err := helper.Prune(ctx, owned, snapshot); err != nil {
			return fmt.Errorf("release Hygon vdev: %w; prune after verified absence: %v", releaseErr, err)
		}
	}
	return os.RemoveAll(dir)
}

func checkHygonCreatePending(dir string) error {
	_, err := os.Lstat(filepath.Join(dir, hygonVdevCreatePendingName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect pending Hygon vdev creation: %w", err)
	}
	return errors.New("unclaimed Hygon vdev creation requires manual recovery")
}

func verifyHygonConfigSnapshot(instanceDir, hostConfig string, hostData []byte) error {
	if hostData == nil {
		var err error
		hostData, err = os.ReadFile(hostConfig)
		if err != nil {
			return fmt.Errorf("read host config: %w", err)
		}
	}
	snapshot, err := os.ReadFile(filepath.Join(instanceDir, filepath.Base(hostConfig)))
	if err != nil {
		return fmt.Errorf("read owned config: %w", err)
	}
	if !bytes.Equal(snapshot, hostData) {
		return fmt.Errorf("host config differs from the owned snapshot")
	}
	return nil
}

func readHygonOwnedVdev(dir string) (hygonOwnedVdev, bool, error) {
	data, err := os.ReadFile(filepath.Join(dir, hygonVdevLedgerName))
	if os.IsNotExist(err) {
		return hygonOwnedVdev{}, false, nil
	}
	if err != nil {
		return hygonOwnedVdev{}, false, err
	}
	var owned hygonOwnedVdev
	if err := json.Unmarshal(data, &owned); err != nil || owned.ID < 0 || len(owned.OwnerToken) != 32 || owned.GPUIndex < 0 || owned.CUCount <= 0 || owned.MemoryMiB <= 0 || owned.PCIeBDF == "" {
		return hygonOwnedVdev{}, false, fmt.Errorf("invalid Hygon vdev ownership in %s", dir)
	}
	return owned, true, nil
}

func parseHygonVdevConfig(raw string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			fields[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}
	return fields
}

type hygonCLIHelper struct{ path string }

func (h hygonCLIHelper) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.path, args...)
	libraryPath := "/opt/hyhal/lib:/opt/dtk/lib"
	if inherited := strings.TrimSpace(os.Getenv("LD_LIBRARY_PATH")); inherited != "" {
		libraryPath += ":" + inherited
	}
	cmd.Env = append(os.Environ(), "LD_LIBRARY_PATH="+libraryPath)
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

func (h hygonCLIHelper) Create(ctx context.Context, gpuIndex, cuCount, memoryMiB int, recordPath, pciBDF string) (int, error) {
	out, err := h.run(ctx, "create", "--gpu-index", strconv.Itoa(gpuIndex), "--cu-count", strconv.Itoa(cuCount), "--memory-mib", strconv.Itoa(memoryMiB), "--record-path", recordPath, "--pci-bdf", pciBDF)
	if err != nil {
		return 0, err
	}
	var response struct {
		VdevID *int `json:"vdev_id"`
	}
	if err := json.Unmarshal(out, &response); err != nil || response.VdevID == nil || *response.VdevID < 0 {
		return 0, fmt.Errorf("invalid Hygon helper create response: %q", out)
	}
	return *response.VdevID, nil
}

func (h hygonCLIHelper) Inspect(ctx context.Context, owned hygonOwnedVdev) (hygonVdevInfo, bool, error) {
	id := owned.ID
	out, err := h.run(ctx, "inspect", "--vdev-id", strconv.Itoa(id), "--owner-token", owned.OwnerToken)
	if err != nil {
		return hygonVdevInfo{}, false, err
	}
	var response struct {
		VdevID    *int    `json:"vdev_id"`
		Exists    *bool   `json:"exists"`
		GPUIndex  *int    `json:"gpu_index"`
		CUCount   *int    `json:"cu_count"`
		MemoryMiB *int    `json:"memory_mib"`
		PCIBDF    *string `json:"pci_bdf"`
	}
	if err := json.Unmarshal(out, &response); err != nil || response.VdevID == nil || *response.VdevID != id || response.Exists == nil {
		return hygonVdevInfo{}, false, fmt.Errorf("invalid Hygon helper inspect response: %q", out)
	}
	if !*response.Exists {
		return hygonVdevInfo{}, false, nil
	}
	if response.GPUIndex == nil || response.CUCount == nil || response.MemoryMiB == nil || response.PCIBDF == nil || *response.GPUIndex < 0 || *response.CUCount <= 0 || *response.MemoryMiB <= 0 || *response.PCIBDF == "" {
		return hygonVdevInfo{}, false, fmt.Errorf("incomplete Hygon helper inspect response: %q", out)
	}
	return hygonVdevInfo{GPUIndex: *response.GPUIndex, CUCount: *response.CUCount, MemoryMiB: *response.MemoryMiB, PCIeBDF: *response.PCIBDF}, true, nil
}

func (h hygonCLIHelper) Release(ctx context.Context, owned hygonOwnedVdev, snapshotPath string) error {
	_, err := h.run(ctx, "release", "--vdev-id", strconv.Itoa(owned.ID), "--owner-token", owned.OwnerToken, "--gpu-index", strconv.Itoa(owned.GPUIndex), "--cu-count", strconv.Itoa(owned.CUCount), "--memory-mib", strconv.Itoa(owned.MemoryMiB), "--pci-bdf", owned.PCIeBDF, "--snapshot-path", snapshotPath)
	return err
}

func (h hygonCLIHelper) Prune(ctx context.Context, owned hygonOwnedVdev, snapshotPath string) error {
	_, err := h.run(ctx, "prune", "--vdev-id", strconv.Itoa(owned.ID), "--owner-token", owned.OwnerToken, "--snapshot-path", snapshotPath)
	return err
}
