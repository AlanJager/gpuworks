//go:build cloudgpu && runpod

package l4

import (
	"strconv"
	"strings"
	"testing"
)

// runpodMetricQuery asks the instance for exactly the fields the agent's
// collector parses; the field positions in runpodMetricQueryFields belong to
// this query and must move with it.
const runpodMetricQuery = "nvidia-smi --query-gpu=index,uuid,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw --format=csv,noheader,nounits 2>&1 || true"

var runpodMetricQueryFields = map[string]int{
	"gpuworks_gpu_utilization":         2, // utilization.gpu
	"gpuworks_gpu_memory_used_bytes":   3, // memory.used
	"gpuworks_gpu_temperature_celsius": 5, // temperature.gpu
}

// parseReadableGPUMetrics mirrors the agent collector's per-field parse
// (internal/agent/gpu): a series exists for a metric only if some valid GPU
// row holds a numeric value at that metric's field position. sawGPURow is
// false when no row looked like a GPU row at all — an nvidia-smi error banner
// is not evidence about the fields.
func parseReadableGPUMetrics(out string) (readable map[string]bool, sawGPURow bool) {
	readable = map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(line, ",")
		if len(parts) < 7 {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(parts[0])); err != nil {
			continue
		}
		sawGPURow = true
		for metric, pos := range runpodMetricQueryFields {
			if _, err := strconv.ParseFloat(strings.TrimSpace(parts[pos]), 64); err == nil {
				readable[metric] = true
			}
		}
	}
	return readable, sawGPURow
}

// The probe decides which metric waits testRunpodMetrics may skip, so both
// directions deserve proof against real shapes: the host-sliced (MIG) output
// observed on the RunPod RTX PRO 6000 Blackwell MIG 1g.24gb SKU, where only
// temperature and power are readable, and the healthy full-GPU row.
func TestParseReadableGPUMetrics(t *testing.T) {
	readable, sawGPURow := parseReadableGPUMetrics("0, GPU-ff013b0b-3ad2-a58c-f43e-2066e60f97f3, [N/A], [Insufficient Permissions], [Insufficient Permissions], 45, 290.25\n")
	if !sawGPURow {
		t.Fatal("a GPU row with unreadable fields is still a GPU row")
	}
	for _, metric := range []string{"gpuworks_gpu_utilization", "gpuworks_gpu_memory_used_bytes"} {
		if readable[metric] {
			t.Fatalf("%s must not be readable when the field is [N/A]/[Insufficient Permissions]", metric)
		}
	}
	if !readable["gpuworks_gpu_temperature_celsius"] {
		t.Fatal("temperature must stay readable on a host-sliced GPU")
	}

	readable, sawGPURow = parseReadableGPUMetrics("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n")
	if sawGPURow || len(readable) != 0 {
		t.Fatalf("an nvidia-smi error banner is not a GPU row: saw_gpu_row=%t readable=%v", sawGPURow, readable)
	}

	readable, sawGPURow = parseReadableGPUMetrics("0, GPU-a, 42, 1024, 8192, 66, 300\n")
	if !sawGPURow || len(readable) != len(runpodMetricQueryFields) {
		t.Fatalf("a healthy row must report every waited-for metric: saw_gpu_row=%t readable=%v", sawGPURow, readable)
	}
}
