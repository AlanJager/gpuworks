package engine

import (
	"context"
	"errors"
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
	"os"
	"path/filepath"
	"testing"
)

func TestNvidiaPartitionMountsHostAndContainerPathsSymmetrically(t *testing.T) {
	state := t.TempDir()
	e := NewWithProber(nil, nil, nil, state, &gpu.FixedProber{VendorName: accelerator.VendorNVIDIA})
	e.extractLibvgpu = func(path string) (string, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(path, []byte("fake-libvgpu"), 0o755); err != nil {
			return "", err
		}
		return path, nil
	}
	cacheDir := filepath.Join(state, "dgpu-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(cacheDir, "instance-a.cache")
	if err := os.WriteFile(old, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := runtime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), &ws, proto.InstanceDesiredState{UUID: "instance-a", GPUIndexes: []int{0, 1}, VRAMLimitBytes: 4 << 30, SMLimitPct: 50, PartitionBackend: accelerator.BackendNVIDIALogical})
	if !ok {
		t.Fatal(reason)
	}
	if info, err := os.Stat(old); err != nil {
		t.Fatalf("fresh cache was not created: %v", err)
	} else if info.Size() != 0 {
		t.Fatalf("fresh cache retained stale data: size=%d", info.Size())
	} else if info.Mode().Perm() != 0o666 {
		t.Fatalf("fresh cache mode=%o, want 0666", info.Mode().Perm())
	}
	if ws.Env["LD_PRELOAD"] != "/gpuworks/libvgpu.so" {
		t.Fatalf("LD_PRELOAD=%q", ws.Env["LD_PRELOAD"])
	}
	if ws.Env["CUDA_DEVICE_MEMORY_SHARED_CACHE"] != "/gpuworks/cache/instance-a.cache" {
		t.Fatalf("cache env=%q", ws.Env["CUDA_DEVICE_MEMORY_SHARED_CACHE"])
	}
	if len(ws.Mounts) != 2 || ws.Mounts[0].Source != filepath.Join(state, "dgpu", "libvgpu.so") || ws.Mounts[0].Target != "/gpuworks/libvgpu.so" || ws.Mounts[1].Source != cacheDir || ws.Mounts[1].Target != "/gpuworks/cache" {
		t.Fatalf("mounts=%+v", ws.Mounts)
	}
	info, err := os.Stat(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o777 || info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("dGPU cache mode=%v, want sticky 0777", info.Mode())
	}
}

func TestNvidiaPartitionFailsClosedWhenLibMissing(t *testing.T) {
	e := NewWithProber(nil, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: accelerator.VendorNVIDIA})
	e.extractLibvgpu = func(string) (string, error) { return "", errors.New("missing fake lib") }
	ok, reason := e.applyPartition(context.Background(), &runtime.WorkloadSpec{Env: map[string]string{}}, proto.InstanceDesiredState{UUID: "missing", GPUIndexes: []int{0}, VRAMLimitBytes: 1, PartitionBackend: accelerator.BackendNVIDIALogical})
	if ok || reason == "" {
		t.Fatalf("nvidia partition=(%v,%q), want fail-closed", ok, reason)
	}
}

func TestNvidiaPartitionFailsClosedWithoutAssignedGPU(t *testing.T) {
	e := NewWithProber(nil, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: accelerator.VendorNVIDIA})
	e.extractLibvgpu = func(path string) (string, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, []byte("fake"), 0o755)
	}
	ok, reason := e.applyPartition(context.Background(), &runtime.WorkloadSpec{Env: map[string]string{}}, proto.InstanceDesiredState{UUID: "no-gpu", VRAMLimitBytes: 1, PartitionBackend: accelerator.BackendNVIDIALogical})
	if ok || reason == "" {
		t.Fatalf("nvidia partition=(%v,%q), want fail-closed", ok, reason)
	}
}

// A desired state without a backend string (older server) must keep defaulting
// to the NVIDIA backend rather than failing the instance.
func TestNvidiaPartitionIsTheDefaultBackend(t *testing.T) {
	e := NewWithProber(nil, nil, nil, t.TempDir(), &gpu.FixedProber{VendorName: accelerator.VendorNVIDIA})
	e.extractLibvgpu = func(path string) (string, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		return path, os.WriteFile(path, []byte("fake"), 0o755)
	}
	ws := runtime.WorkloadSpec{Env: map[string]string{}}
	ok, reason := e.applyPartition(context.Background(), &ws, proto.InstanceDesiredState{UUID: "default-backend", GPUIndexes: []int{0}, VRAMLimitBytes: 1 << 30})
	if !ok {
		t.Fatal(reason)
	}
	if ws.Env["LD_PRELOAD"] != "/gpuworks/libvgpu.so" {
		t.Fatalf("LD_PRELOAD=%q, want the NVIDIA libvgpu injection", ws.Env["LD_PRELOAD"])
	}
}
