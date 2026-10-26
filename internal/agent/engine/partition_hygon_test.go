package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

type hygonTestDriver struct {
	agentruntime.Driver
	name string
}

func (d hygonTestDriver) Name() string { return d.name }

type fakeHygonVdevHelper struct {
	vdevDir                                                    string
	nextID                                                     int
	owned                                                      map[int]bool
	ownerTokens                                                map[int]string
	identities                                                 map[int]hygonVdevInfo
	createErr, releaseErr, releaseAfterDestroyErr, responseErr error
	createdCUs                                                 []int
	released                                                   []int
	pruneHook                                                  func(int)
}

func (f *fakeHygonVdevHelper) Create(_ context.Context, index, cus, memoryMiB int, recordPath, pciBDF string) (int, error) {
	if f.createErr != nil {
		return 0, f.createErr
	}
	id := f.nextID
	f.nextID++
	if f.owned == nil {
		f.owned = map[int]bool{}
	}
	f.owned[id] = true
	if f.ownerTokens == nil {
		f.ownerTokens = map[int]string{}
	}
	f.ownerTokens[id] = fmt.Sprintf("%032x", len(f.createdCUs)+1)
	if f.identities == nil {
		f.identities = map[int]hygonVdevInfo{}
	}
	f.identities[id] = hygonVdevInfo{GPUIndex: index, CUCount: cus, MemoryMiB: memoryMiB, PCIeBDF: pciBDF}
	f.createdCUs = append(f.createdCUs, cus)
	config := fmt.Sprintf("PciBusId: 0000:07:00.0\ncu_mask: 0x%016x\ncu_mask: 0x0000000000000000\ncu_count: %d\nmem: %d MiB\ndevice_id: %d\nvdev_id: %d\npipe_id: %d\nenable: 1\n", (uint64(1)<<uint(cus))-1, cus, memoryMiB, index, id, id)
	name := fmt.Sprintf("vdev%d.conf", id)
	if err := os.WriteFile(filepath.Join(f.vdevDir, name), []byte(config), 0o644); err != nil {
		return 0, err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(recordPath), name), []byte(config), 0o644); err != nil {
		return 0, err
	}
	ledger, err := json.Marshal(hygonOwnedVdev{ID: id, OwnerToken: f.ownerTokens[id], hygonVdevInfo: f.identities[id]})
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(recordPath, append(ledger, '\n'), 0o600); err != nil {
		return 0, err
	}
	if f.responseErr != nil {
		return 0, f.responseErr
	}
	return id, nil
}
func (f *fakeHygonVdevHelper) Inspect(_ context.Context, owned hygonOwnedVdev) (hygonVdevInfo, bool, error) {
	if owned.OwnerToken != f.ownerTokens[owned.ID] {
		return hygonVdevInfo{}, false, errors.New("Hygon vdev belongs to a different creation")
	}
	return f.identities[owned.ID], f.owned[owned.ID], nil
}
func (f *fakeHygonVdevHelper) Release(_ context.Context, owned hygonOwnedVdev, snapshotPath string) error {
	if owned.OwnerToken != f.ownerTokens[owned.ID] {
		return errors.New("Hygon vdev belongs to a different creation")
	}
	if f.releaseErr != nil {
		return f.releaseErr
	}
	id := owned.ID
	if f.owned[id] && !owned.matches(f.identities[id]) {
		return errors.New("Hygon vdev identity mismatch")
	}
	host, err := os.ReadFile(filepath.Join(f.vdevDir, fmt.Sprintf("vdev%d.conf", id)))
	if err != nil {
		return err
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		return err
	}
	if string(host) != string(snapshot) {
		return errors.New("Hygon vdev config differs from owned snapshot")
	}
	delete(f.owned, id)
	delete(f.identities, id)
	f.released = append(f.released, id)
	if err := os.Remove(filepath.Join(f.vdevDir, fmt.Sprintf("vdev%d.conf", id))); err != nil {
		return err
	}
	return f.releaseAfterDestroyErr
}

func (f *fakeHygonVdevHelper) Prune(_ context.Context, owned hygonOwnedVdev, snapshotPath string) error {
	id := owned.ID
	if owned.OwnerToken != f.ownerTokens[id] {
		return errors.New("Hygon vdev belongs to a different creation")
	}
	if f.pruneHook != nil {
		f.pruneHook(id)
	}
	if f.owned[id] {
		return errors.New("Hygon vdev appeared before stale config pruning")
	}
	hostPath := filepath.Join(f.vdevDir, fmt.Sprintf("vdev%d.conf", id))
	host, err := os.ReadFile(hostPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	snapshot, err := os.ReadFile(snapshotPath)
	if err != nil {
		return err
	}
	if string(host) != string(snapshot) {
		return errors.New("Hygon vdev config differs from owned snapshot")
	}
	return os.Remove(hostPath)
}

func hygonTestEngine(t *testing.T) (*Engine, hygonPartitionInjector, *fakeHygonVdevHelper) {
	t.Helper()
	root := t.TempDir()
	h := hygonPartitionInjector{vdevDir: filepath.Join(root, "vdev"), dtkDir: filepath.Join(root, "dtk"), hyhalDir: filepath.Join(root, "hyhal"), helperPath: filepath.Join(root, "gpuworks-hygon-vdev-helper"), lockPath: filepath.Join(root, ".dmi_virtual.lock")}
	for _, path := range []string{filepath.Join(h.dtkDir, "lib", "libamdhip64.so"), filepath.Join(h.dtkDir, "lib", "librocm_smi64.so"), filepath.Join(h.hyhalDir, "lib", "libhydmi.so"), filepath.Join(h.hyhalDir, "lib", "libhydmi_mig.so"), h.helperPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test asset"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(h.vdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.lockPath, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	fake := &fakeHygonVdevHelper{vdevDir: h.vdevDir, nextID: 4}
	h.helper = fake
	e := &Engine{stateDir: root, driver: hygonTestDriver{name: "docker"}, prober: &gpu.FixedProber{
		VendorName: accelerator.VendorHygon,
		GPUs:       []proto.GPUInfo{{Index: 0, Vendor: accelerator.VendorHygon, ArchID: "Z100", PCIeBDF: "0000:07:00.0", MemoryTotalBytes: 32752 << 20}},
	}}
	return e, h, fake
}

func TestHygonCreatesVendorVdevAndCleansOnlyOwnedID(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	apply := func(uuid string) *agentruntime.WorkloadSpec {
		t.Helper()
		ws := &agentruntime.WorkloadSpec{Image: "hygon/vllm:dtk25", Env: map[string]string{"LD_LIBRARY_PATH": "/image/lib"}}
		ok, reason := h.apply(e, context.Background(), ws, proto.InstanceDesiredState{UUID: uuid, GPUIndexes: []int{0}, VRAMLimitBytes: 1537 << 20})
		if !ok {
			t.Fatalf("apply %s: %s", uuid, reason)
		}
		return ws
	}
	first := apply("first")
	second := apply("second")
	if len(fake.createdCUs) != 2 || fake.createdCUs[0] != 3 || fake.createdCUs[1] != 3 {
		t.Fatalf("proportional CUs = %v", fake.createdCUs)
	}
	if len(first.Mounts) != 5 || len(second.Mounts) != 5 {
		t.Fatalf("mounts = %+v / %+v", first.Mounts, second.Mounts)
	}
	if first.Mounts[0].Target != "/etc/vdev/vdev4.conf" || second.Mounts[0].Target != "/etc/vdev/vdev5.conf" || first.Mounts[0].Source != first.Mounts[1].Source || !first.Mounts[0].ReadOnly || !first.Mounts[1].ReadOnly || first.Mounts[1].Target != "/etc/vdev/docker/vdev0.conf" || second.Mounts[1].Target != "/etc/vdev/docker/vdev0.conf" || first.Mounts[1].Source == second.Mounts[1].Source {
		t.Fatalf("instance vdev mounts = %+v / %+v", first.Mounts[:2], second.Mounts[:2])
	}
	if first.Mounts[2] != (agentruntime.Mount{Source: h.lockPath, Target: hygonVirtualLock}) || second.Mounts[2] != first.Mounts[2] {
		t.Fatalf("shared vendor lock mount = %+v / %+v", first.Mounts[2], second.Mounts[2])
	}
	for index, want := range []agentruntime.Mount{
		{Source: h.dtkDir, Target: "/opt/hygondriver", ReadOnly: true},
		{Source: h.hyhalDir, Target: "/opt/hyhal", ReadOnly: true},
	} {
		if first.Mounts[index+3] != want || second.Mounts[index+3] != want {
			t.Fatalf("runtime mount %d = %+v / %+v, want %+v", index, first.Mounts[index+3], second.Mounts[index+3], want)
		}
	}
	if !strings.HasPrefix(first.Env["LD_LIBRARY_PATH"], "/opt/dtk/lib:/opt/dtk/lib64:/opt/hyhal/lib:") || !strings.HasSuffix(first.Env["LD_LIBRARY_PATH"], ":/image/lib") {
		t.Fatalf("library path = %q", first.Env["LD_LIBRARY_PATH"])
	}
	config, err := os.ReadFile(first.Mounts[1].Source)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "mem: 1537 MiB") || !strings.Contains(string(config), "cu_count: 3") {
		t.Fatalf("config = %s", config)
	}
	if dir, err := os.Stat(filepath.Dir(first.Mounts[1].Source)); err != nil || dir.Mode().Perm()&0o005 != 0o005 {
		t.Fatalf("non-root cannot traverse config dir: %v/%v", dir, err)
	}
	if file, err := os.Stat(first.Mounts[1].Source); err != nil || file.Mode().Perm()&0o004 == 0 {
		t.Fatalf("non-root cannot read config: %v/%v", file, err)
	}
	_ = apply("first") // restart reuses the owned vendor ID
	if fake.nextID != 6 {
		t.Fatalf("restart created another vendor device: next=%d", fake.nextID)
	}
	if err := h.cleanupInstance(e, "first"); err != nil {
		t.Fatal(err)
	}
	if len(fake.released) != 1 || fake.released[0] != 4 || !fake.owned[5] {
		t.Fatalf("release scope = %v, owned = %v", fake.released, fake.owned)
	}
}

func TestHygonProbeMountsHostDTK(t *testing.T) {
	e, h, _ := hygonTestEngine(t)
	ws := &agentruntime.WorkloadSpec{Image: assets.PartitionProbeImage}
	ok, reason := h.apply(e, context.Background(), ws, proto.InstanceDesiredState{UUID: "probe", GPUIndexes: []int{0}, VRAMLimitBytes: 4096 << 20})
	if !ok {
		t.Fatal(reason)
	}
	if got, want := ws.Mounts[len(ws.Mounts)-1], (agentruntime.Mount{Source: h.dtkDir, Target: "/opt/dtk", ReadOnly: true}); got != want {
		t.Fatalf("probe DTK mount = %+v, want %+v", got, want)
	}
}

func TestHygonFailsClosedAndPreservesLedgerOnReleaseFailure(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "test", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	ws := &agentruntime.WorkloadSpec{}
	e.driver = hygonTestDriver{name: "nerdctl"}
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "Docker") {
		t.Fatalf("runtime: %v/%s", ok, reason)
	}
	e.driver = hygonTestDriver{name: "docker"}
	prober := e.prober.(*gpu.FixedProber)
	prober.GPUs[0].ArchID = "K100"
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "Z100") {
		t.Fatalf("model: %v/%s", ok, reason)
	}
	prober.GPUs[0].ArchID = "Z100"
	spec.VRAMLimitBytes = math.MaxInt64
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "physical VRAM") {
		t.Fatalf("quota: %v/%s", ok, reason)
	}
	spec.VRAMLimitBytes = 1024 << 20
	spec.SMLimitPct = 25
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "compute quota") {
		t.Fatalf("compute: %v/%s", ok, reason)
	}
	spec.SMLimitPct = 0
	if err := os.Remove(filepath.Join(h.dtkDir, "lib", "libamdhip64.so")); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "asset") {
		t.Fatalf("asset: %v/%s", ok, reason)
	}
	if err := os.WriteFile(filepath.Join(h.dtkDir, "lib", "libamdhip64.so"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake.createErr = errors.New("DCGM no free CUs")
	if ok, reason := h.apply(e, context.Background(), ws, spec); ok || !strings.Contains(reason, "no free CUs") {
		t.Fatalf("create: %v/%s", ok, reason)
	}
	fake.createErr = nil
	if ok, reason := h.apply(e, context.Background(), ws, spec); !ok {
		t.Fatalf("create: %s", reason)
	}
	fake.releaseErr = errors.New("DCGM still busy")
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("cleanup = %v", err)
	}
	if _, found, err := readHygonOwnedVdev(hygonInstanceDir(e, spec.UUID)); err != nil || !found {
		t.Fatalf("ownership lost after failed cleanup: %v/%v", found, err)
	}
	fake.releaseErr = nil
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatal(err)
	}
}

func TestHygonCleanupAcceptsStopErrorAfterVerifiedDestroy(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "stop-error", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	fake.releaseAfterDestroyErr = errors.New("StopVDevice failed after destroy")
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatalf("verified absent device should be cleaned: %v", err)
	}
	if fake.owned[4] {
		t.Fatal("native vdev remains")
	}
	if _, err := os.Stat(hygonInstanceDir(e, spec.UUID)); !os.IsNotExist(err) {
		t.Fatalf("ownership directory remains: %v", err)
	}
}

func TestHygonStaleLedgerCannotReuseOrReleaseRecreatedID(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "old-creation", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	fake.ownerTokens[4] = "ffffffffffffffffffffffffffffffff"
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "different creation") {
		t.Fatalf("stale cleanup error=%v", err)
	}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "different creation") {
		t.Fatalf("stale reuse result=%t reason=%s", ok, reason)
	}
	if !fake.owned[4] || len(fake.released) != 0 {
		t.Fatal("old ledger touched the replacement")
	}
	if _, found, err := readHygonOwnedVdev(hygonInstanceDir(e, spec.UUID)); err != nil || !found {
		t.Fatalf("old ledger lost after rejection: found=%t error=%v", found, err)
	}
}

func TestHygonRejectsMissingSharedLockWithoutLosingVdevOwnership(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	if err := os.Remove(h.lockPath); err != nil {
		t.Fatal(err)
	}
	spec := proto.InstanceDesiredState{UUID: "missing-lock", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "lock unavailable") {
		t.Fatalf("missing lock accepted: %v/%s", ok, reason)
	}
	if !fake.owned[4] {
		t.Fatal("native vdev ownership was lost after injection failed")
	}
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatal(err)
	}
	if fake.owned[4] {
		t.Fatal("failed injection left a native vdev behind")
	}
}

func TestHygonRefusesReusedForeignVdevID(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "reused", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	fake.identities[4] = hygonVdevInfo{GPUIndex: 0, CUCount: 10, MemoryMiB: 2048}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "different device") {
		t.Fatalf("reapply foreign device = %v/%s", ok, reason)
	}
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "different device") {
		t.Fatalf("cleanup foreign device = %v", err)
	}
	if len(fake.released) != 0 || !fake.owned[4] {
		t.Fatalf("foreign vdev destroyed: released=%v owned=%v", fake.released, fake.owned)
	}
}

func TestHygonRefusesReplacedConfigWithSameDriverIdentity(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "replaced-config", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	hostConfig := filepath.Join(h.vdevDir, "vdev4.conf")
	config, err := os.ReadFile(hostConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostConfig, append(config, []byte("foreign_owner: 1\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "configuration changed") {
		t.Fatalf("reapply changed config = %v/%s", ok, reason)
	}
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("cleanup changed config = %v", err)
	}
	if len(fake.released) != 0 || !fake.owned[4] {
		t.Fatalf("foreign vdev destroyed: released=%v owned=%v", fake.released, fake.owned)
	}
}

func TestHygonRemovesOnlyVerifiedConfigWhenDriverDeviceIsAbsent(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "absent", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	fake.owned[4] = false // driver reset removed the device, leaving its config
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.vdevDir, "vdev4.conf")); !os.IsNotExist(err) {
		t.Fatalf("verified stale config remains: %v", err)
	}
	if len(fake.released) != 0 {
		t.Fatalf("absent device released: %v", fake.released)
	}
}

func TestHygonPruneRechecksDeviceAfterItReappears(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "reappeared", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); !ok {
		t.Fatal(reason)
	}
	fake.owned[4] = false
	fake.pruneHook = func(id int) { fake.owned[id] = true }
	if err := h.cleanupInstance(e, spec.UUID); err == nil || !strings.Contains(err.Error(), "appeared") {
		t.Fatalf("cleanup after ID reuse = %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.vdevDir, "vdev4.conf")); err != nil {
		t.Fatalf("live replacement config removed: %v", err)
	}
	if _, found, err := readHygonOwnedVdev(hygonInstanceDir(e, spec.UUID)); err != nil || !found {
		t.Fatalf("ownership lost after unsafe prune: %v/%v", found, err)
	}
}

func TestHygonDoesNotCreateOverUnclaimedState(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "unclaimed", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	dir := hygonInstanceDir(e, spec.UUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vdev4.conf"), []byte("orphan snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "manual recovery") {
		t.Fatalf("unclaimed state = %v/%s", ok, reason)
	}
	if fake.nextID != 4 {
		t.Fatalf("created another native device: next=%d", fake.nextID)
	}
}

func TestHygonCleansUpCreatedVdevAfterLostHelperResponse(t *testing.T) {
	e, h, fake := hygonTestEngine(t)
	spec := proto.InstanceDesiredState{UUID: "lost-response", GPUIndexes: []int{0}, VRAMLimitBytes: 1024 << 20}
	fake.responseErr = errors.New("helper response was lost")
	if ok, reason := h.apply(e, context.Background(), &agentruntime.WorkloadSpec{}, spec); ok || !strings.Contains(reason, "response was lost") {
		t.Fatalf("first apply = %v/%s", ok, reason)
	}
	if _, found, err := readHygonOwnedVdev(hygonInstanceDir(e, spec.UUID)); err != nil || !found {
		t.Fatalf("durable ownership = %v/%v", found, err)
	}
	// Reconcile cleans up a failed apply before reporting failure. The durable
	// ledger lets that cleanup release the device even when stdout was lost.
	if err := h.cleanupInstance(e, spec.UUID); err != nil {
		t.Fatalf("cleanup after lost response: %v", err)
	}
	if fake.owned[4] || len(fake.released) != 1 || fake.released[0] != 4 {
		t.Fatalf("created device was not released: owned=%v released=%v", fake.owned, fake.released)
	}
	if _, err := os.Stat(hygonInstanceDir(e, spec.UUID)); !os.IsNotExist(err) {
		t.Fatalf("ownership directory remains after cleanup: %v", err)
	}
}

func TestHygonAssetReportRequiresNativeHelperAndLibraries(t *testing.T) {
	e, h, _ := hygonTestEngine(t)
	if reports := h.assetReports(e); len(reports) != 1 || reports[0].Backend != accelerator.BackendHygonLogical || len(reports[0].Files) != 5 {
		t.Fatalf("reports = %+v", reports)
	}
	if err := os.Remove(h.helperPath); err != nil {
		t.Fatal(err)
	}
	if reports := h.assetReports(e); len(reports) != 0 {
		t.Fatalf("missing helper reported: %+v", reports)
	}
}
