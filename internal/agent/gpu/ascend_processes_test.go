package gpu

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// withAscendProcessRows replaces the "No running processes found in NPU n"
// row of a real table with process rows, the way npu-smi prints a chip that
// is in use.
func withAscendProcessRows(t *testing.T, table string, npu string, rows ...string) string {
	t.Helper()
	marker := "| No running processes found in NPU " + npu + " "
	for _, line := range strings.Split(table, "\n") {
		if strings.HasPrefix(line, marker) {
			return strings.Replace(table, line, strings.Join(rows, "\n"), 1)
		}
	}
	t.Fatalf("fixture has no idle row for NPU %s", npu)
	return ""
}

func TestParseAscendProcessesOneChipCards(t *testing.T) {
	table := withAscendProcessRows(t, readNPUFixture(t, "npu-smi-info-910b4.txt"), "5",
		"| 5       0                 | 41234         | python3            | 30215                 | 87                      |",
		"| 5       0                 | 41240         | VLLM::Worker TP0   | 2048                  | 93                      |")
	want := []ComputeProcess{
		{PID: 41234, Name: "python3", PCIeBDF: "0000:02:00.0", MemoryBytes: 30215 << 20},
		{PID: 41240, Name: "VLLM::Worker TP0", PCIeBDF: "0000:02:00.0", MemoryBytes: 2048 << 20},
	}
	if got := parseAscendProcesses(table); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAscendProcesses = %+v, want %+v", got, want)
	}
}

// The row exactly as npu-smi 26.0.rc1 printed it on 172.30.8.31 (2026-10-30)
// for a host python3 holding 512 MiB through ACL on NPU 4; a process outside
// any container reads "NA" in the last column.
func TestParseAscendProcessesRealRow(t *testing.T) {
	table := withAscendProcessRows(t, readNPUFixture(t, "npu-smi-info-910b4.txt"), "6",
		"| 6       0                 | 600747        | python3            | 626                   | NA                      |")
	want := []ComputeProcess{{PID: 600747, Name: "python3", PCIeBDF: "0000:41:00.0", MemoryBytes: 626 << 20}}
	if got := parseAscendProcesses(table); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAscendProcesses = %+v, want %+v", got, want)
	}
}

func TestParseAscendProcessesMultiChipCards(t *testing.T) {
	// 910C: each NPU has chips 0 and 1 with their own Bus-Id; a process row
	// names the chip by (NPU, chip), not by Phy-ID. Older releases have no
	// container column.
	table := withAscendProcessRows(t, readNPUFixture(t, "npu-smi-info-910c-warning.txt"), "3",
		"| 3       1                 | 777           | sglang::scheduler  | 51200                 |")
	want := []ComputeProcess{{PID: 777, Name: "sglang::scheduler", PCIeBDF: "0000:93:00.0", MemoryBytes: 51200 << 20}}
	if got := parseAscendProcesses(table); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAscendProcesses = %+v, want %+v", got, want)
	}
}

func TestParseAscendProcessesIdleAndUnknownChips(t *testing.T) {
	if got := parseAscendProcesses(readNPUFixture(t, "npu-smi-info-910b4.txt")); len(got) != 0 {
		t.Fatalf("idle table listed %+v", got)
	}
	// A row for a chip the card table does not list (lent to a VM between
	// the two tables, or a malformed row) names no card and is dropped.
	table := withAscendProcessRows(t, readNPUFixture(t, "npu-smi-info-910b4.txt"), "1",
		"| 3       0                 | 9             | python             | 10                    | 1                       |",
		"| 1       0                 | n/a           | python             | 10                    | 1                       |")
	if got := parseAscendProcesses(table); len(got) != 0 {
		t.Fatalf("unmatched rows listed %+v", got)
	}
}

func TestAscendComputeProcessesUsesCachedInfo(t *testing.T) {
	table := withAscendProcessRows(t, readNPUFixture(t, "npu-smi-info-910b4.txt"), "7",
		"| 7       0                 | 100           | python             | 1                     | 2                       |")
	calls := 0
	p := &AscendProber{runner: func(_ context.Context, args ...string) (string, error) {
		calls++
		if len(args) == 1 && args[0] == "info" {
			return table, nil
		}
		return "", errors.New("unexpected " + strings.Join(args, " "))
	}}
	if _, err := p.Probe(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := calls
	procs, err := p.ComputeProcesses(t.Context())
	if err != nil || len(procs) != 1 || procs[0].PCIeBDF != "0000:42:00.0" {
		t.Fatalf("ComputeProcesses = %+v, %v", procs, err)
	}
	if calls != before {
		t.Fatalf("listing processes ran npu-smi again (%d calls after the probe's %d)", calls, before)
	}
}

func TestAscendComputeProcessesError(t *testing.T) {
	p := &AscendProber{runner: func(context.Context, ...string) (string, error) {
		return "", errors.New("dcmi module initialize failed")
	}}
	if procs, err := p.ComputeProcesses(t.Context()); err == nil || procs != nil {
		t.Fatalf("a failed npu-smi must not read as no processes: %+v, %v", procs, err)
	}
}
