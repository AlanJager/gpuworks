package gpu

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseComputeProcesses(t *testing.T) {
	// Real ppu-smi output from the PPU host (2026-10-10), plus a name with a
	// comma and rows the tool prints when nothing runs.
	out := "135707, sglang::scheduler, 00000000:7E:00.0\n" +
		"1741701, VLLM::StageEngineCoreProc_noid_replica0_DP0, 00000001:C8:00.0\n" +
		"42, python, worker, 00000000:65:00.0\n" +
		"No running processes found\n\n"
	want := []ComputeProcess{
		{PID: 135707, Name: "sglang::scheduler", PCIeBDF: "00000000:7E:00.0"},
		{PID: 1741701, Name: "VLLM::StageEngineCoreProc_noid_replica0_DP0", PCIeBDF: "00000001:C8:00.0"},
		{PID: 42, Name: "python, worker", PCIeBDF: "00000000:65:00.0"},
	}
	if got := parseComputeProcesses(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseComputeProcesses = %+v", got)
	}
}

func TestSameBDF(t *testing.T) {
	if !SameBDF("00000000:7E:00.0", "0000:7e:00.0") || SameBDF("0000:7e:00.0", "0000:7f:00.0") || SameBDF("", "") {
		t.Fatal("SameBDF")
	}
}

func TestNVIDIAComputeProcessesQueryAndError(t *testing.T) {
	var args []string
	p := &SMIProber{runner: func(_ context.Context, a ...string) (string, error) {
		args = a
		return "7, vllm, 00000000:65:00.0\n", nil
	}}
	procs, err := p.ComputeProcesses(t.Context())
	if err != nil || len(procs) != 1 || procs[0].PID != 7 || !strings.HasPrefix(args[0], "--query-compute-apps=pid,process_name,gpu_bus_id") {
		t.Fatalf("procs = %+v, %v, args %q", procs, err, args)
	}
	p.runner = func(context.Context, ...string) (string, error) { return "", errors.New("nvml unavailable") }
	if _, err := p.ComputeProcesses(t.Context()); err == nil {
		t.Fatal("error swallowed")
	}
}
