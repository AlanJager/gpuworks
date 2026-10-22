package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// fakeDiagnosticTools puts nvidia-smi and python3 stand-ins first on PATH.
// Each records its call in the returned log; python3 answers as a passing
// check would.
func fakeDiagnosticTools(t *testing.T) (log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	for name, body := range map[string]string{
		"nvidia-smi": "echo nvidia-smi >> " + log + "\necho 580.126.09\n",
		"python3":    "echo python3 >> " + log + "\necho PASS fake\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func readCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

func stubCUDACheck(t *testing.T, result string) {
	t.Helper()
	orig := doctorCUDACheck
	doctorCUDACheck = func(context.Context) string { return result }
	t.Cleanup(func() { doctorCUDACheck = orig })
}

func fact(r proto.DoctorReport, code string) string {
	v, _ := factValue(r, code)
	return v
}

// On zstone-164 a card that failed to initialize holds every nvidia-smi on
// the driver lock for up to a minute and cuInit returns 101. Doctor then
// takes the driver version from the inventory instead of asking nvidia-smi
// again, and does not run peer access and NCCL on a driver the CUDA check
// already found unusable: together they ran the report past the server's
// wait.
func TestDoctorNVIDIAChecksSpareASlowBrokenDriver(t *testing.T) {
	log := fakeDiagnosticTools(t)
	stubCUDACheck(t, "FAIL: cuInit=101")
	r := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", Driver: "580.126.09"}}

	doctorNVIDIAChecks(context.Background(), &r, 4)

	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("doctor ran %v; want neither nvidia-smi nor the python checks", calls)
	}
	if fabric := fact(r, proto.DoctorFactFabricManager); !strings.HasPrefix(fabric, "driver=580.126.09 ") || !strings.Contains(fabric, "fabric_manager=") {
		t.Fatalf("fabric fact = %q", fabric)
	}
	for _, code := range []string{proto.DoctorFactPeerAccess, proto.DoctorFactNCCL} {
		if got := fact(r, code); got != cudaDependentSkip {
			t.Fatalf("%s = %q, want %q", code, got, cudaDependentSkip)
		}
	}
	var codes []string
	for _, f := range r.Findings {
		codes = append(codes, f.Code)
	}
	if len(codes) != 1 || codes[0] != proto.DoctorCUDAInitFailed {
		t.Fatalf("findings = %v, want only %s", codes, proto.DoctorCUDAInitFailed)
	}
}

// The gate only spares a driver the CUDA check failed on: after a pass the
// multi-GPU checks run, and with one card they still say why they skip.
func TestDoctorNVIDIAChecksRunMultiGPUChecksAfterCUDAPasses(t *testing.T) {
	log := fakeDiagnosticTools(t)
	stubCUDACheck(t, "PASS cuInit=ok")
	r := proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia", Driver: "580.126.09"}}
	doctorNVIDIAChecks(context.Background(), &r, 2)
	if calls := readCalls(t, log); len(calls) != 2 || calls[0] != "python3" || calls[1] != "python3" {
		t.Fatalf("calls = %v, want the peer access and NCCL checks", calls)
	}
	if got := fact(r, proto.DoctorFactPeerAccess); got != "PASS fake" {
		t.Fatalf("peer access = %q", got)
	}

	stubCUDACheck(t, "FAIL: cuInit=101")
	r = proto.DoctorReport{Accelerators: &proto.DoctorAccelerators{Vendor: "nvidia"}}
	doctorNVIDIAChecks(context.Background(), &r, 1)
	if got := fact(r, proto.DoctorFactPeerAccess); got != "SKIP (fewer than 2 GPUs)" {
		t.Fatalf("one-card peer access = %q", got)
	}
}

// deadlineProber records the deadline doctor probes under.
type deadlineProber struct {
	gpu.FixedProber
	deadline time.Time
	ok       bool
}

func (p *deadlineProber) Probe(ctx context.Context) ([]proto.GPUInfo, error) {
	p.deadline, p.ok = ctx.Deadline()
	return nil, nil
}

// A doctor the server asked for ends within its own budget, so the report
// reaches the server before the server stops waiting for it.
func TestDoctorRPCRunsWithinItsBudget(t *testing.T) {
	p := &deadlineProber{}
	n := &NodeRuntime{prober: p}
	start := time.Now()
	result, err := n.OnRPC(context.Background(), proto.RPCNodeDoctor, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(proto.DoctorReport); !ok {
		t.Fatalf("result = %T", result)
	}
	end := time.Now()
	if !p.ok || p.deadline.Before(start.Add(doctorBudget)) || p.deadline.After(end.Add(doctorBudget)) {
		t.Fatalf("probe deadline = %v after the call (set %v), want %s", p.deadline.Sub(start), p.ok, doctorBudget)
	}
}
