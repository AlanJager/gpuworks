package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const usageMiB = uint64(1) << 20

// shimLedger is a region as the packaged shim writes it (see
// internal/agent/dgpucache/testdata/ledger_fixture.c).
func shimLedger(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "dgpucache", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fillLedger writes into the ledger file the way the workload does: through
// the file the container was given, without replacing it.
func fillLedger(t *testing.T, path string, data []byte) {
	t.Helper()
	fillLedgerAt(t, path, data, 0)
}

func fillLedgerAt(t *testing.T, path string, data []byte, offset int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(data, offset); err != nil {
		t.Fatal(err)
	}
}

// restartGetsAFreshLedger starts the instance again while something still
// has the last start's ledger open, as a container being replaced may. The
// new start must not inherit the old charges - a ledger charges processes by
// PID, and a new container's PIDs repeat the old one's - and the old file
// must not be truncated under a process that still maps it.
func restartGetsAFreshLedger(t *testing.T, e *Engine, spec proto.InstanceDesiredState, source string, restart func() (bool, string)) {
	t.Helper()
	old, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	before, err := old.Stat()
	if err != nil || before.Size() == 0 {
		t.Fatalf("ledger before the restart = %v, %v; want the shim's region", before, err)
	}
	if ok, reason := restart(); !ok {
		t.Fatal(reason)
	}
	fresh, err := os.Stat(source)
	if err != nil || fresh.Size() != 0 || os.SameFile(before, fresh) {
		t.Fatalf("ledger after a restart = %v, %v; want a new empty file", fresh, err)
	}
	if after, err := old.Stat(); err != nil || after.Size() != before.Size() {
		t.Fatalf("the last start's ledger = %v, %v; want it left as it was", after, err)
	}
	if devices, _, err := e.DGPUUsage(spec); !errors.Is(err, dgpucache.ErrCacheUninitialized) || devices != nil {
		t.Fatalf("usage after a restart = %+v, %v; want none until the new engine allocates", devices, err)
	}
}

func partitionAssets(t *testing.T, root, backend string, names ...string) {
	t.Helper()
	dir := filepath.Join(root, "dgpu-runtime", backend, "linux-"+runtime.GOARCH)
	if backend == "ppu" {
		dir = filepath.Join(dir, "hggc-13.0")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(dir, name), fakeELF(runtime.GOARCH), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func mountAt(ws *agentruntime.WorkloadSpec, target string) *agentruntime.Mount {
	for i := range ws.Mounts {
		if ws.Mounts[i].Target == target {
			return &ws.Mounts[i]
		}
	}
	return nil
}

// An Ascend slice's memory ledger is a POSIX shared memory object inside the
// container. The agent backs its name with a file of its own, so what the
// slice holds can be read whether or not the container shares the host's
// /dev/shm, and gives every start a new one.
func TestAscendSliceUsageIsReadFromTheLedgerTheAgentBacks(t *testing.T) {
	root := t.TempDir()
	partitionAssets(t, root, "ascend", "libvruntime.so")
	prober := &ascendPartitionTestProber{FixedProber: &gpu.FixedProber{VendorName: "ascend", FrameworkName: "cann", FrameworkVersion: "9.0.0"}, hardwareID: "AAAA BBBB"}
	e := &Engine{stateDir: root, prober: prober}
	// A first slice takes virtual NPU 0 on the card; this one gets 1.
	other := proto.InstanceDesiredState{UUID: "other", GPUIndexes: []int{4}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendAscendLogical}
	if ok, reason := e.applyPartition(context.Background(), &agentruntime.WorkloadSpec{Env: map[string]string{}}, other); !ok {
		t.Fatal(reason)
	}
	spec := proto.InstanceDesiredState{UUID: "ascend-1", GPUIndexes: []int{4}, VRAMLimitBytes: 8 << 30, SMLimitPct: 50, PartitionBackend: accelerator.BackendAscendLogical}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	injector := ascendPartitionInjector{}
	if ok, reason := injector.applyWithUsageMount(e, context.Background(), ws, spec, true); !ok {
		t.Fatal(reason)
	}
	config, err := os.ReadFile(filepath.Join(root, "dgpu-runtime", "instances", "ascend-1", "npu_info.config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "shm-id=AAAA-BBBB\n") || !strings.Contains(string(config), "virtual-npu-id=1\n") {
		t.Fatalf("config = %q", config)
	}
	// vcann-rt opens "/enpu-vmem-<shm-id>-<virtual-npu-id>".
	mount := mountAt(ws, "/dev/shm/enpu-vmem-AAAA-BBBB-1")
	if mount == nil || mount.ReadOnly {
		t.Fatalf("ledger mount = %+v in %+v, want a writable one at the name the shim opens", mount, ws.Mounts)
	}
	if mount.Source != filepath.Join(root, "dgpu-runtime", "instances", "ascend-1", "usage", "ledger") {
		t.Fatalf("ledger source = %q, want a file in the instance's partition state", mount.Source)
	}
	info, err := os.Stat(mount.Source)
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("ledger file = %v, %v; want an empty file any container user can write", info, err)
	}
	if info, err := os.Stat(filepath.Dir(mount.Source)); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("ledger parent = %v, %v; want a private directory on the host", info, err)
	}

	// Nothing allocated yet: no figure, and nothing worth logging.
	if devices, _, err := e.DGPUUsage(spec); !errors.Is(err, dgpucache.ErrCacheUninitialized) || devices != nil {
		t.Fatalf("usage before the engine allocates = %+v, %v", devices, err)
	}
	fillLedger(t, mount.Source, shimLedger(t, "ascend-ledger-v1.bin"))
	devices, compute, err := e.DGPUUsage(spec)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].MemoryUsedBytes != 6860*usageMiB {
		t.Fatalf("devices = %+v", devices)
	}
	if compute {
		t.Fatal("an Ascend slice has no compute figure, whatever its compute limit")
	}

	restartGetsAFreshLedger(t, e, spec, mount.Source, func() (bool, string) {
		return injector.applyWithUsageMount(e, context.Background(), &agentruntime.WorkloadSpec{}, spec, true)
	})

	if err := e.cleanupPartitionState(spec.UUID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.DGPUUsage(spec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("usage after teardown = %v, want the ledger gone", err)
	}
}

func TestAscendSliceUsageReadsTheExistingHostIPCObject(t *testing.T) {
	root := t.TempDir()
	partitionAssets(t, root, "ascend", "libvruntime.so")
	e := &Engine{stateDir: root, hostRoot: t.TempDir()}
	spec := proto.InstanceDesiredState{UUID: "ascend-unmounted", GPUIndexes: []int{4}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendAscendLogical}
	ws := &agentruntime.WorkloadSpec{}
	injector := ascendPartitionInjector{}
	if ok, reason := injector.applyWithUsageMount(e, context.Background(), ws, spec, canMountAscendUsageLedger(1000, nil)); !ok {
		t.Fatal(reason)
	}
	target := "/dev/shm/enpu-vmem-card-4-0"
	if mount := mountAt(ws, target); mount != nil {
		t.Fatalf("non-root agent must leave the original IPC path alone: %+v", mount)
	}
	if _, _, err := e.DGPUUsage(spec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("usage before the shim allocates = %v, want no figure", err)
	}
	path := filepath.Join(e.hostRoot, target)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, shimLedger(t, "ascend-ledger-v1.bin"), 0o600); err != nil {
		t.Fatal(err)
	}
	devices, compute, err := e.DGPUUsage(spec)
	if err != nil || len(devices) != 1 || devices[0].MemoryUsedBytes != 6860*usageMiB || compute {
		t.Fatalf("usage = %+v compute=%v, %v; want memory from the host IPC ledger", devices, compute, err)
	}
}

// A PPU slice's ledger path is the shim's usage surface. Outside Kubernetes
// the agent backs it with a file of its own for the same reason as Ascend's.
func TestPPUSliceUsageIsReadFromTheLedgerTheAgentBacks(t *testing.T) {
	root := t.TempDir()
	partitionAssets(t, root, "ppu", "hggc_quota.so", "hgml_dlsym_hook.so")
	e := &Engine{stateDir: root}
	spec := proto.InstanceDesiredState{UUID: "ppu-usage-test-1", GPUIndexes: []int{7, 9}, VRAMLimitBytes: 8 << 30, SMLimitPct: 50, PartitionBackend: accelerator.BackendPPULogical}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	if ok, reason := e.applyPartition(context.Background(), ws, spec); !ok {
		t.Fatal(reason)
	}
	ledger := ws.Env["HGGC_LEDGER_PATH"]
	if ledger != "/opt/gpuworks/partition/usage-ledger" {
		t.Fatalf("HGGC_LEDGER_PATH = %q", ledger)
	}
	mount := mountAt(ws, ledger)
	if mount == nil || mount.ReadOnly || mount.Source != filepath.Join(root, "dgpu-runtime", "instances", "ppu-usage-test-1", "usage", "ledger") {
		t.Fatalf("ledger mount = %+v in %+v", mount, ws.Mounts)
	}
	info, err := os.Stat(mount.Source)
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o666 {
		t.Fatalf("ledger file = %v, %v", info, err)
	}
	if devices, _, err := e.DGPUUsage(spec); !errors.Is(err, dgpucache.ErrCacheUninitialized) || devices != nil {
		t.Fatalf("usage before the engine allocates = %+v, %v", devices, err)
	}
	fillLedger(t, mount.Source, shimLedger(t, "ppu-ledger-v1.bin"))
	devices, compute, err := e.DGPUUsage(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []dgpucache.DeviceUsage{{MemoryUsedBytes: 7168 * usageMiB, SMUtilPercent: 37}, {MemoryUsedBytes: 2048*usageMiB + 4096}}
	if len(devices) != 2 || devices[0] != want[0] || devices[1] != want[1] || !compute {
		t.Fatalf("usage = %+v compute=%v, want %+v with the capped slice's compute", devices, compute, want)
	}
	// The shim's controller measures nothing for a slice whose compute is
	// not capped, so the zero it leaves is not a reading.
	for _, limit := range []int{0, 100} {
		uncapped := spec
		uncapped.SMLimitPct = limit
		if _, compute, err := e.DGPUUsage(uncapped); err != nil || compute {
			t.Fatalf("sm limit %d: compute=%v err=%v, want memory only", limit, compute, err)
		}
	}
	restartGetsAFreshLedger(t, e, spec, mount.Source, func() (bool, string) {
		return e.applyPartition(context.Background(), &agentruntime.WorkloadSpec{}, spec)
	})
}

// On Kubernetes the ledger stays the host /dev/shm file the pod mounts, and
// is read through the agent's view of the host.
func TestPPUSliceUsageOnKubeReadsTheHostLedger(t *testing.T) {
	root, hostRoot := t.TempDir(), t.TempDir()
	partitionAssets(t, root, "ppu", "hggc_quota.so", "hgml_dlsym_hook.so")
	e := &Engine{stateDir: root, hostRoot: hostRoot}
	spec := proto.InstanceDesiredState{UUID: "ppu-kube-usage", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, SMLimitPct: 50, PartitionBackend: accelerator.BackendPPULogical}
	ws := &agentruntime.WorkloadSpec{Env: map[string]string{}}
	if ok, reason := e.applyPartition(context.Background(), ws, spec); !ok {
		t.Fatal(reason)
	}
	ledger := "/dev/shm/gpuworks-ppu-ppu-kube-usage"
	if mount := mountAt(ws, ledger); mount == nil || mount.Source != ledger {
		t.Fatalf("ledger mount = %+v, want the host path itself", mount)
	}
	fillLedger(t, filepath.Join(hostRoot, ledger), shimLedger(t, "ppu-ledger-v1.bin"))
	devices, compute, err := e.DGPUUsage(spec)
	if err != nil || len(devices) != 1 || devices[0].MemoryUsedBytes != 7168*usageMiB || devices[0].SMUtilPercent != 37 || !compute {
		t.Fatalf("usage = %+v compute=%v err=%v", devices, compute, err)
	}
}

// NVIDIA is as before: the HAMi-core cache, memory and SM utilization both,
// and an older server's desired state names no backend.
func TestNvidiaSliceUsageIsReadFromTheHAMiCache(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	for _, backend := range []string{accelerator.BackendNVIDIALogical, ""} {
		spec := proto.InstanceDesiredState{UUID: "nvidia-1", GPUIndexes: []int{3}, VRAMLimitBytes: 8 << 30, PartitionBackend: backend}
		if err := os.RemoveAll(filepath.Dir(e.DGPUCachePath(spec.UUID))); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.DGPUUsage(spec); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("backend %q: usage without a cache = %v", backend, err)
		}
		if err := os.MkdirAll(filepath.Dir(e.DGPUCachePath(spec.UUID)), 0o755); err != nil {
			t.Fatal(err)
		}
		hamiCache(t, e.DGPUCachePath(spec.UUID), 8<<30, 7700*usageMiB)
		// The process's SM utilization on device 0 (shared_region_t v1.2).
		sm := make([]byte, 8)
		binary.LittleEndian.PutUint64(sm, 64)
		fillLedgerAt(t, e.DGPUCachePath(spec.UUID), sm, 1856+1160+16)
		devices, compute, err := e.DGPUUsage(spec)
		if err != nil || len(devices) != 1 || devices[0] != (dgpucache.DeviceUsage{MemoryUsedBytes: 7700 * usageMiB, SMUtilPercent: 64}) || !compute {
			t.Fatalf("backend %q: usage = %+v compute=%v err=%v", backend, devices, compute, err)
		}
	}
}

// A backend that keeps no per-slice figure, and an instance that is not a
// slice at all, have none - which is not an error to log every ten seconds.
func TestSliceUsageIsAbsentWhereNoBackendKeepsIt(t *testing.T) {
	e := &Engine{stateDir: t.TempDir()}
	for _, spec := range []proto.InstanceDesiredState{
		{UUID: "hygon", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendHygonLogical},
		{UUID: "amd", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendAMDLogical},
		{UUID: "unknown", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: "no-such-dgpu"},
		{UUID: "whole-card", GPUIndexes: []int{0}},
		{UUID: "no-card", VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendNVIDIALogical},
	} {
		if devices, compute, err := e.DGPUUsage(spec); devices != nil || compute || err != nil {
			t.Fatalf("%s: usage = %+v compute=%v err=%v, want none", spec.UUID, devices, compute, err)
		}
	}
}

// The agent and the server decide from the same registry which readings a
// backend has: an injector that can read usage belongs to a backend that
// declares it, and the other way round.
func TestPartitionUsageReadersMatchTheRegistry(t *testing.T) {
	for id, injector := range partitionInjectors {
		backend, ok := accelerator.Lookup(id)
		if !ok {
			t.Fatalf("%s is not in the registry", id)
		}
		_, reads := injector.(partitionUsageReader)
		if memory, _ := accelerator.SliceUsage(backend, 50); memory != reads {
			t.Errorf("%s: registry says memory usage %v, the injector reads it %v", id, memory, reads)
		}
	}
}
