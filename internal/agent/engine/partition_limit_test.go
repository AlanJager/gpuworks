package engine

import (
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// hamiCache writes what HAMi-core's first process leaves in an instance's
// shared cache: the v1.2 header, the limit on device 0, and one process
// holding heldBytes there. The offsets are dgpucache's (shared_region_t v1.2).
func hamiCache(t *testing.T, path string, limit, heldBytes uint64) {
	t.Helper()
	const processBase, processSize, processCountAt = 1856, 1960, 1856 + 1024*1960
	data := make([]byte, processCountAt+4)
	binary.LittleEndian.PutUint32(data[0:], 19920718)
	binary.LittleEndian.PutUint32(data[4:], 1)
	binary.LittleEndian.PutUint32(data[8:], 2)
	binary.LittleEndian.PutUint64(data[1600:], limit)
	binary.LittleEndian.PutUint32(data[processCountAt:], 1)
	binary.LittleEndian.PutUint32(data[processBase:], 100)
	binary.LittleEndian.PutUint64(data[processBase+8+32:], heldBytes)
	if err := os.WriteFile(path, data, 0o666); err != nil {
		t.Fatal(err)
	}
}

func cacheLimit(t *testing.T, path string) uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return binary.LittleEndian.Uint64(data[1600:])
}

func dgpuEngine(t *testing.T) (*Engine, *statusLog, *livenessDriver) {
	t.Helper()
	driver := newLivenessDriver()
	log := &statusLog{}
	e := NewWithProber(driver, log.report, slog.Default(), t.TempDir(), injectionProber{})
	e.extractLibvgpu = func(path string) (string, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, []byte("fake-libvgpu"), 0o755)
	}
	return e, log, driver
}

// A running slice whose desired state names a new memory limit is held to it
// in place: the agent adopts the container it already runs and rewrites the
// limit in the slice's shared cache. The container is not started again.
func TestRunningSliceTakesANewLimitInPlace(t *testing.T) {
	e, log, driver := dgpuEngine(t)
	spec := runningSpec("live-limit", healthyEngine(t))
	spec.GPUIndexes, spec.VRAMLimitBytes, spec.PartitionBackend = []int{0}, 6<<30, accelerator.BackendNVIDIALogical
	serve(t, e, log, spec)
	cache := e.DGPUCachePath(spec.UUID)
	hamiCache(t, cache, 6<<30, 5<<30)

	raised := spec
	raised.VRAMLimitBytes = 9 << 30
	e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{raised}, nil)
	waitIdle(t, e, spec.UUID)
	if got := cacheLimit(t, cache); got != 9<<30 {
		t.Fatalf("limit after raise = %d, want 9 GiB", got)
	}

	// Below what the slice holds, the limit would fail the engine's next
	// allocation: it is left for a restart.
	lowered := spec
	lowered.VRAMLimitBytes = 4 << 30
	e.ApplyUpdate(context.Background(), []proto.InstanceDesiredState{lowered}, nil)
	waitIdle(t, e, spec.UUID)
	if got := cacheLimit(t, cache); got != 9<<30 {
		t.Fatalf("limit below usage was written: %d", got)
	}
	driver.mu.Lock()
	runs := driver.runs
	driver.mu.Unlock()
	if runs != 1 {
		t.Fatalf("container started %d times, want once", runs)
	}
	if failed := log.states(proto.InstFailed); len(failed) != 0 {
		t.Fatalf("a limit change failed the instance: %+v", failed)
	}
}

// A backend that reads its limit only at start is left alone: its cache or
// config is not touched and the instance keeps running.
func TestStartTimeBackendKeepsItsLimitUntilRestart(t *testing.T) {
	e, _, _ := dgpuEngine(t)
	spec := proto.InstanceDesiredState{UUID: "ppu-slice", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendPPULogical}
	if changed, err := partitionInjectors[accelerator.BackendPPULogical].applyLimit(e, spec); changed || err != errLiveLimitUnsupported {
		t.Fatalf("ppu applyLimit = %v, %v", changed, err)
	}
	if changed, err := partitionInjectors[accelerator.BackendAscendLogical].applyLimit(e, spec); changed || err != errLiveLimitUnsupported {
		t.Fatalf("ascend applyLimit = %v, %v", changed, err)
	}
	for _, backend := range accelerator.All() {
		_, live := partitionInjectors[backend.ID()].applyLimit(e, proto.InstanceDesiredState{UUID: "probe", GPUIndexes: []int{0}, VRAMLimitBytes: 1 << 30, PartitionBackend: backend.ID()})
		if (live != errLiveLimitUnsupported) != accelerator.LiveMemoryLimit(backend) {
			t.Errorf("%s: agent applyLimit and accelerator.LiveMemoryLimit disagree (err=%v)", backend.ID(), live)
		}
	}
	// Nothing to hold yet: no cache, or one HAMi-core has not set up.
	e.reconcilePartitionLimit(proto.InstanceDesiredState{UUID: "no-cache", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if err := os.MkdirAll(filepath.Dir(e.DGPUCachePath("empty")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.DGPUCachePath("empty"), make([]byte, 1856+1024*1960+4), 0o666); err != nil {
		t.Fatal(err)
	}
	e.reconcilePartitionLimit(proto.InstanceDesiredState{UUID: "empty", GPUIndexes: []int{0}, VRAMLimitBytes: 8 << 30, PartitionBackend: accelerator.BackendNVIDIALogical})
	if got := cacheLimit(t, e.DGPUCachePath("empty")); got != 0 {
		t.Fatalf("an uninitialised cache was written: %d", got)
	}
}
