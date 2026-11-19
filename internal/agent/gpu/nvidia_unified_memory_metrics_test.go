package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GWKS-417: a unified-memory part answers [N/A] for memory.used and
// memory.total, so the card's use was never reported and the console showed
// 0 in use. Both figures come from one read of the host's meminfo, marked as
// the host pool; a figure the driver answers, and every discrete card, is
// left as it is. The fake answers the exact queries the prober sends, one
// value per requested column.
func TestMetricsFillUnifiedMemoryFromTheHostPool(t *testing.T) {
	const (
		totalKiB, availKiB = 127598832, 100000000
		hostInfo           = "MemTotal:       127598832 kB\nMemFree:        1024 kB\nMemAvailable:   100000000 kB\n"
		spark              = "NVIDIA RTX Spark N1X (6144-core Blackwell RTX GPU)"
		usedKey, totalKey  = "gpuworks_gpu_memory_used_bytes", "gpuworks_gpu_memory_total_bytes"
		uuid               = "GPU-e78d5a9c-a30c-482a-a004-123456789abc"
	)
	absent := float64(-1)
	for _, tc := range []struct {
		name, model, row, meminfo string
		used, total               float64 // absent when the metric must not be reported
		hostPool, noRow           bool
	}{
		{name: "Thor answers nothing", model: "NVIDIA Thor", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo,
			used: (totalKiB - availKiB) * 1024, total: totalKiB * 1024, hostPool: true},
		{name: "RTX Spark answers load but no memory", model: spark, row: "37, [N/A], [N/A], 52, 31.20, [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo,
			used: (totalKiB - availKiB) * 1024, total: totalKiB * 1024, hostPool: true},
		{name: "GB10 answers its own memory", model: "NVIDIA GB10", row: "12, 2048, 119444, 50, 20.00, [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo,
			used: 2048 << 20, total: 119444 << 20},
		{name: "GB10 answers a total but no use", model: "NVIDIA GB10", row: "12, [N/A], 1024, 50, 20.00, [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo,
			used: 1024 << 20, total: 1024 << 20, hostPool: true}, // never more than the card's total
		{name: "discrete card keeps its missing memory", model: "NVIDIA GeForce RTX 5090", row: "12, [N/A], [N/A], 50, 20.00, [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo,
			used: absent, total: absent},
		{name: "discrete card that answers nothing has no row", model: "NVIDIA RTX A6000", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: hostInfo, noRow: true},
		{name: "kernel without MemAvailable fills the total only", model: "NVIDIA Thor", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]",
			meminfo: "MemTotal:       127598832 kB\nMemFree:        1024 kB\n", used: absent, total: totalKiB * 1024},
		{name: "MemAvailable above MemTotal is not believed", model: "NVIDIA Thor", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]",
			meminfo: "MemTotal:       1000 kB\nMemAvailable:   2000 kB\n", used: absent, total: 1000 * 1024},
		{name: "host meminfo missing", model: "NVIDIA Thor", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]", noRow: true},
		{name: "host MemTotal malformed", model: "NVIDIA Thor", row: "[N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]", meminfo: "MemTotal: unavailable kB\nMemAvailable: 5 kB\n", noRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := unifiedMemoryProber(t, tc.meminfo, tc.model, uuid, tc.row)
			if _, err := p.Probe(t.Context()); err != nil {
				t.Fatal(err)
			}
			metrics, err := p.Metrics(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if tc.noRow {
				if len(metrics) != 0 {
					t.Fatalf("metrics = %+v, want none", metrics)
				}
				return
			}
			if len(metrics) != 1 || metrics[0].UUID != uuid {
				t.Fatalf("metrics = %+v", metrics)
			}
			m := metrics[0]
			for key, want := range map[string]float64{usedKey: tc.used, totalKey: tc.total} {
				got, ok := m.Values[key]
				if want == absent {
					if ok {
						t.Fatalf("%s = %v, want it absent", key, got)
					}
					continue
				}
				if !ok || got != want {
					t.Fatalf("%s = %v (reported %v), want %v", key, got, ok, want)
				}
			}
			if m.HostPoolMemory != tc.hostPool {
				t.Fatalf("HostPoolMemory = %v, want %v", m.HostPoolMemory, tc.hostPool)
			}
		})
	}
}

// Before any probe the prober does not know which cards are unified memory,
// so nothing is filled in on a guess.
func TestMetricsBeforeAProbeFillNothing(t *testing.T) {
	p := unifiedMemoryProber(t, "MemTotal: 1000 kB\nMemAvailable: 400 kB\n", "NVIDIA Thor", "GPU-thor", "55, [N/A], [N/A], 50, 20.00, [N/A], [N/A], [N/A], [N/A], [N/A]")
	metrics, err := p.Metrics(t.Context())
	if err != nil || len(metrics) != 1 {
		t.Fatalf("metrics = %+v, %v", metrics, err)
	}
	if _, ok := metrics[0].Values["gpuworks_gpu_memory_used_bytes"]; ok || metrics[0].HostPoolMemory {
		t.Fatalf("filled memory for a card not yet identified: %+v", metrics[0])
	}
}

// unifiedMemoryProber is a one-card host: it answers the inventory query, the
// listing and the top metrics query, and refuses anything else. row is the
// metrics row after index and uuid.
func unifiedMemoryProber(t *testing.T, meminfo, model, uuid, row string) *SMIProber {
	t.Helper()
	root := t.TempDir()
	if meminfo != "" {
		if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "proc/meminfo"), []byte(meminfo), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const format = "--format=csv,noheader,nounits"
	return &SMIProber{hostRoot: root, runner: func(_ context.Context, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case nvidiaInventoryQueries[0] + " " + format:
			return fmt.Sprintf("0, %s, %s, [N/A], 12.1, 0000000F:01:00.0, 595.78, [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]\n", uuid, model), nil
		case "-L":
			return fmt.Sprintf("GPU 0: %s (UUID: %s)\n", model, uuid), nil
		case metricsQueryEventReasons + " " + format:
			return fmt.Sprintf("0, %s, %s\n", uuid, row), nil
		case "dmon -c 1 -s t":
			return "", fmt.Errorf("dmon is not supported on this platform")
		default:
			t.Errorf("unexpected nvidia-smi command: %q", args)
			return "", fmt.Errorf("unsupported nvidia-smi command: %q", args)
		}
	}}
}
