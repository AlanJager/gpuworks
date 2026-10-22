package placement

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func resolvePrefer(t *testing.T, call string, count int) (Result, error) {
	t.Helper()
	source := "def resolve(gpu):\n    return " + call + "\n"
	if err := Compile(source); err != nil {
		return Result{}, err
	}
	return Resolve(context.Background(), source, GPU{Count: count, MinMemoryGiB: 80})
}

func TestAcceptPreferDefaultsToZero(t *testing.T) {
	result, err := resolvePrefer(t, "accept(gpu_memory_mib_per_device=1024)", 4)
	if err != nil || result.Prefer != 0 || result.GPUMemoryMiBPerDevice != 1024 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	// A bare variable dictionary states no preference either.
	result, err = resolvePrefer(t, `{"GPU_COUNT": gpu.count}`, 4)
	if err != nil || result.Prefer != 0 {
		t.Fatalf("dictionary result=%+v err=%v", result, err)
	}
}

func TestAcceptPreferFollowsTheGPUCount(t *testing.T) {
	for _, call := range []string{
		"accept(1024, {}, gpu.count)",
		"accept(gpu_memory_mib_per_device=1024, prefer=gpu.count)",
		"accept(1024, prefer=gpu.count, variables={\"GPU_COUNT\": gpu.count})",
	} {
		for _, count := range []int{4, 8} {
			result, err := resolvePrefer(t, call, count)
			if err != nil || result.Prefer != int64(count) {
				t.Fatalf("%s with %d GPUs: result=%+v err=%v", call, count, result, err)
			}
		}
	}
	result, err := resolvePrefer(t, "accept(1024, prefer=-gpu.count)", 8)
	if err != nil || result.Prefer != -8 {
		t.Fatalf("negative preference: result=%+v err=%v", result, err)
	}
}

func TestAcceptPreferAcceptsTheBoundsAndRejectsBeyond(t *testing.T) {
	for _, value := range []int64{MaxPreference, -MaxPreference} {
		result, err := resolvePrefer(t, fmt.Sprintf("accept(1024, prefer=%d)", value), 1)
		if err != nil || result.Prefer != value {
			t.Fatalf("prefer=%d: result=%+v err=%v", value, result, err)
		}
	}
	for _, value := range []int64{MaxPreference + 1, -MaxPreference - 1} {
		_, err := resolvePrefer(t, fmt.Sprintf("accept(1024, prefer=%d)", value), 1)
		if err == nil || !strings.Contains(err.Error(), "prefer must be within") {
			t.Fatalf("prefer=%d: err=%v", value, err)
		}
	}
}

func TestAcceptPreferRejectsNonIntegers(t *testing.T) {
	for _, value := range []string{`"8"`, "2.5", "True", "None", "[8]"} {
		if _, err := resolvePrefer(t, "accept(1024, prefer="+value+")", 1); err == nil {
			t.Fatalf("prefer=%s was accepted", value)
		}
	}
}
