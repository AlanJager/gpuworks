package engine

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLibvgpuInjectionUsesContainerPreloadAndPerGPULimits(t *testing.T) {
	env := buildLibvgpuInjection("/gpuworks/libvgpu.so", "instance-a", "/gpuworks/cache", 4, 8<<30, 50)
	if got := env["LD_PRELOAD"]; got != "/gpuworks/libvgpu.so" {
		t.Fatalf("LD_PRELOAD = %q", got)
	}
	if strings.Contains(env["LD_PRELOAD"], "state") {
		t.Fatal("LD_PRELOAD leaked a host path")
	}
	for i := 0; i < 4; i++ {
		if got := env["CUDA_DEVICE_MEMORY_LIMIT_"+strconv.Itoa(i)]; got != strconv.FormatInt(8<<30, 10) {
			t.Fatalf("memory limit %d = %q", i, got)
		}
	}
	if _, ok := env["CUDA_DEVICE_MEMORY_LIMIT_4"]; ok {
		t.Fatal("memory limit emitted for an invisible GPU")
	}
	if env["CUDA_DEVICE_SM_LIMIT"] != "50" {
		t.Fatalf("SM limits = %#v", env)
	}
	// HAMi-core reads no such variable; its limiter is on by default.
	if _, ok := env["CUDA_DEVICE_SM_LIMIT_FORCE"]; ok {
		t.Fatal("injected a variable HAMi-core does not read")
	}
	if got, want := env["CUDA_DEVICE_MEMORY_SHARED_CACHE"], filepath.Join("/gpuworks/cache", "instance-a.cache"); got != want {
		t.Fatalf("cache path = %q, want %q", got, want)
	}
}

func TestLibvgpuInjectionOmitsSMLimitAtBounds(t *testing.T) {
	for _, pct := range []int{0, 100, 150, -1} {
		if _, ok := buildLibvgpuInjection("/gpuworks/libvgpu.so", "id", "/cache", 1, 1, pct)["CUDA_DEVICE_SM_LIMIT"]; ok {
			t.Fatalf("SM limit present for pct=%d", pct)
		}
	}
}
