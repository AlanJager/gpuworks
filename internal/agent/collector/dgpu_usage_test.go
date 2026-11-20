package collector

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/dgpucache"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Each dGPU slice reports its own memory, whatever its backend, and its
// compute utilization only where the backend measures one: an Ascend or
// uncapped PPU slice must not report a 0% it never measured. A slice with
// nothing to read yet reports nothing - not zero - and a read that failed is
// neither a sample nor the end of the batch.
func TestDGPUSamplesReportEachSliceItsBackendCanRead(t *testing.T) {
	const gib = 1 << 30
	read := func(devices []dgpucache.DeviceUsage, compute bool, err error) func() ([]dgpucache.DeviceUsage, bool, error) {
		return func() ([]dgpucache.DeviceUsage, bool, error) { return devices, compute, err }
	}
	instances := []RunningInstance{
		// NVIDIA, two cards: memory and SM utilization on each.
		{UUID: "nvidia", GPUIndexes: []int{2, 5}, DeploymentID: 7,
			DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: 7 * gib, SMUtilPercent: 40}, {MemoryUsedBytes: 6 * gib, SMUtilPercent: 0}}, true, nil)},
		// Ascend: memory only.
		{UUID: "ascend", GPUIndexes: []int{0}, DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: 6860 << 20}}, false, nil)},
		// PPU with its compute capped, and without.
		{UUID: "ppu-capped", GPUIndexes: []int{1}, DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: 7168 << 20, SMUtilPercent: 37}}, true, nil)},
		{UUID: "ppu-uncapped", GPUIndexes: []int{1}, DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: 7168 << 20}}, false, nil)},
		// An idle slice holds nothing, which is a reading.
		{UUID: "idle", GPUIndexes: []int{3}, DGPUUsage: read([]dgpucache.DeviceUsage{{}}, false, nil)},
		// Nothing to read: the engine has not allocated, the instance predates
		// the ledger, its backend keeps none, or it is a whole-card instance.
		{UUID: "starting", GPUIndexes: []int{0}, DGPUUsage: read(nil, false, dgpucache.ErrCacheUninitialized)},
		{UUID: "predates", GPUIndexes: []int{0}, DGPUUsage: read(nil, false, fmt.Errorf("open ledger: %w", os.ErrNotExist))},
		{UUID: "hygon", GPUIndexes: []int{0}, DGPUUsage: read(nil, false, nil)},
		{UUID: "whole-card", GPUIndexes: []int{4}},
		{UUID: "no-card", DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: gib}}, true, nil)},
		// A region this agent cannot parse, and one naming more cards than
		// the instance has.
		{UUID: "unreadable", GPUIndexes: []int{0}, DGPUUsage: read(nil, false, errors.New("unsupported layout 9"))},
		{UUID: "extra-device", GPUIndexes: []int{6}, DGPUUsage: read([]dgpucache.DeviceUsage{{MemoryUsedBytes: gib}, {MemoryUsedBytes: 2 * gib}}, false, nil)},
	}
	var logs bytes.Buffer
	c := New("node-1", collectorProber{}, func() []RunningInstance { return instances }, func(proto.MetricBatch) {},
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	var got []string
	for _, sample := range c.dgpuSamples(1234) {
		if sample.TimeMS != 1234 || sample.Labels["node"] != "node-1" {
			t.Fatalf("sample = %+v, want this node at the batch's time", sample)
		}
		keys := make([]string, 0, len(sample.Labels))
		for key := range sample.Labels {
			if key != "node" {
				keys = append(keys, key+"="+sample.Labels[key])
			}
		}
		sort.Strings(keys)
		got = append(got, fmt.Sprintf("%s{%s} %d", sample.Metric, strings.Join(keys, ","), int64(sample.Value)))
	}
	want := []string{
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{deployment=7,gpu=2,instance=nvidia} %d", 7*gib),
		"gpuworks_dgpu_sm_util{deployment=7,gpu=2,instance=nvidia} 40",
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{deployment=7,gpu=5,instance=nvidia} %d", 6*gib),
		"gpuworks_dgpu_sm_util{deployment=7,gpu=5,instance=nvidia} 0",
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{gpu=0,instance=ascend} %d", 6860<<20),
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{gpu=1,instance=ppu-capped} %d", 7168<<20),
		"gpuworks_dgpu_sm_util{gpu=1,instance=ppu-capped} 37",
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{gpu=1,instance=ppu-uncapped} %d", 7168<<20),
		"gpuworks_dgpu_memory_used_bytes{gpu=3,instance=idle} 0",
		fmt.Sprintf("gpuworks_dgpu_memory_used_bytes{gpu=6,instance=extra-device} %d", gib),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("samples:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Only the read that failed for a reason worth knowing is logged.
	if text := logs.String(); !strings.Contains(text, "instance=unreadable") || strings.Count(text, "dGPU usage read failed") != 1 {
		t.Fatalf("log = %q, want the unreadable region and nothing else", text)
	}
}
