package gpu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// GWKS-320: the public probe must size the RTX Spark reported in PR #18,
// while a discrete RTX or a host without MemTotal keeps an unknown total.
// Fixtures answer the actual CLI query with one value per requested column;
// an unrelated command must not accidentally succeed with inventory CSV.
func TestProbeSizesUnifiedMemoryCardsFromHostMemTotal(t *testing.T) {
	const hostKiB = 127598832
	const hostInfo = "MemTotal:       127598832 kB\nMemFree:        1024 kB\n"
	const spark = "NVIDIA RTX Spark N1X (6144-core Blackwell RTX GPU)"
	for _, tc := range []struct {
		name, model, memory, compute, meminfo string
		want                                  int64
	}{
		{"RTX Spark unavailable", spark, "[N/A]", "12.1", hostInfo, hostKiB * 1024},
		{"GB10 unavailable", "NVIDIA GB10", "[N/A]", "12.1", hostInfo, hostKiB * 1024},
		{"RTX Spark zero", spark, "0", "12.1", hostInfo, hostKiB * 1024},
		// GWKS-417: the Jetson Thor module's GPU, as the report names it.
		{"Thor unavailable", "NVIDIA Thor", "[N/A]", "12.1", hostInfo, hostKiB * 1024},
		{"Thor look-alike stays unknown", "NVIDIA Thorium", "[N/A]", "12.1", hostInfo, 0},
		{"GB10 reports its memory", "NVIDIA GB10", "119444", "12.1", hostInfo, 119444 << 20},
		{"discrete GeForce RTX", "NVIDIA GeForce RTX 5090", "[N/A]", "12.0", hostInfo, 0},
		{"discrete RTX", "NVIDIA RTX A6000", "[N/A]", "8.6", hostInfo, 0},
		{"discrete RTX PRO", "NVIDIA RTX PRO 6000 Blackwell Server Edition", "[N/A]", "12.0", hostInfo, 0},
		{"discrete RTX reports its memory", "NVIDIA GeForce RTX 4090", "24564", "8.9", hostInfo, 24564 << 20},
		{"host meminfo missing", spark, "[N/A]", "12.1", "", 0},
		{"host MemTotal missing", spark, "[N/A]", "12.1", "MemFree:        1024 kB\n", 0},
		{"host MemTotal malformed", spark, "[N/A]", "12.1", "MemTotal: unavailable kB\n", 0},
		{"host MemTotal zero", spark, "[N/A]", "12.1", "MemTotal: 0 kB\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.meminfo != "" {
				if err := os.MkdirAll(filepath.Join(root, "proc"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "proc/meminfo"), []byte(tc.meminfo), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			const uuid = "GPU-e78d5a9c-a30c-482a-a004-123456789abc"
			const query = "--query-gpu=index,uuid,name,memory.total,compute_cap,pci.bus_id,driver_version,power.limit,ecc.errors.uncorrected.volatile.total,ecc.errors.uncorrected.aggregate.total,retired_pages.pending,remapped_rows.pending,remapped_rows.failure,gpu_recovery_action"
			const format = "--format=csv,noheader,nounits"
			var commands []string
			var p Prober = &SMIProber{hostRoot: root, runner: func(_ context.Context, args ...string) (string, error) {
				commands = append(commands, strings.Join(args, " "))
				switch {
				case reflect.DeepEqual(args, []string{query, format}):
					return fmt.Sprintf("0, %s, %s, %s, %s, 0000000F:01:00.0, 580.95.05, [N/A], [N/A], [N/A], [N/A], [N/A], [N/A], [N/A]\n", uuid, tc.model, tc.memory, tc.compute), nil
				case reflect.DeepEqual(args, []string{"-L"}):
					return fmt.Sprintf("GPU 0: %s (UUID: %s)\n", tc.model, uuid), nil
				default:
					t.Errorf("unexpected nvidia-smi command: %q", args)
					return "", fmt.Errorf("unsupported nvidia-smi command: %q", args)
				}
			}}
			gpus, err := p.Probe(t.Context())
			if err != nil || len(gpus) != 1 {
				t.Fatalf("Probe = %+v, %v", gpus, err)
			}
			if gpus[0].MemoryTotalBytes != tc.want || gpus[0].MIGEnabled || gpus[0].UUID != uuid {
				t.Fatalf("Probe = %+v, want total %d and an unchanged whole-card identity", gpus[0], tc.want)
			}
			if !reflect.DeepEqual(commands, []string{query + " " + format, "-L"}) {
				t.Fatalf("nvidia-smi commands = %q", commands)
			}
		})
	}
}
