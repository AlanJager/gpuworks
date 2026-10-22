package app

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// withDoctorExecSeams overrides checkCmd's exec seams for the duration of the
// test, restoring the real exec.LookPath/exec.CommandContext afterward so
// other tests are unaffected.
func withDoctorExecSeams(t *testing.T, lookPath func(string) (string, error), run func(context.Context, string, ...string) ([]byte, error)) {
	t.Helper()
	origLookPath, origRun := checkDoctorCmdLookPath, checkDoctorCmdRun
	t.Cleanup(func() { checkDoctorCmdLookPath, checkDoctorCmdRun = origLookPath, origRun })
	checkDoctorCmdLookPath = lookPath
	checkDoctorCmdRun = run
	withDoctorMounts(t, mountInfoPlainHost, "")
}

// withDoctorMounts presents the mount table and container marker of a machine
// the test does not run on, so a report never depends on where it is built.
func withDoctorMounts(t *testing.T, mountinfo, containerMarker string) {
	t.Helper()
	origTable, origResolve, origMarker := doctorMountTable, doctorResolvePath, doctorContainerMarker
	t.Cleanup(func() {
		doctorMountTable, doctorResolvePath, doctorContainerMarker = origTable, origResolve, origMarker
	})
	doctorMountTable = func() ([]byte, error) { return []byte(mountinfo), nil }
	doctorResolvePath = func(path string) string { return path }
	doctorContainerMarker = func() string { return containerMarker }
}

// A non-NVIDIA vendor keeps doctorWithProber from also shelling out to real
// sh/python3 for the NVIDIA-only manual diagnostics section, which would
// otherwise make this test slower and dependent on what is installed.
func doctorFixedNonNVIDIAProber() *gpu.FixedProber {
	return &gpu.FixedProber{VendorName: "ascend", GPUs: []proto.GPUInfo{{Index: 0, Vendor: "ascend", Name: "910B", Healthy: true}}}
}

// dcgmi's documented flag is --version; doctor must call it with exactly that
// (not the older/incorrect "-v"), and the report line must be present with
// the command's real output.
func TestDoctorReportsDCGMIPresentUsesVersionFlag(t *testing.T) {
	var gotArgs []string
	withDoctorExecSeams(t,
		func(name string) (string, error) {
			if name == "dcgmi" {
				return "/usr/bin/dcgmi", nil
			}
			return "", exec.ErrNotFound
		},
		func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "dcgmi" {
				return nil, errors.New("unexpected command: " + name)
			}
			gotArgs = append([]string(nil), args...)
			return []byte("dcgmi version: 3.3.9\n"), nil
		},
	)
	var report strings.Builder
	doctorWithProber(context.Background(), &report, doctorFixedNonNVIDIAProber())
	if len(gotArgs) != 1 || gotArgs[0] != "--version" {
		t.Fatalf("dcgmi args = %+v, want exactly [--version]", gotArgs)
	}
	if !strings.Contains(report.String(), "dcgmi     3.3.9\n") {
		t.Fatalf("doctor report lacks the dcgmi version line:\n%s", report.String())
	}
}

// A node with no dcgmi binary at all reports "not found" for that line and
// never attempts to run it.
func TestDoctorReportsDCGMIAbsent(t *testing.T) {
	ran := false
	withDoctorExecSeams(t,
		func(string) (string, error) { return "", exec.ErrNotFound },
		func(context.Context, string, ...string) ([]byte, error) {
			ran = true
			return nil, errors.New("must not be called")
		},
	)
	var report strings.Builder
	doctorWithProber(context.Background(), &report, doctorFixedNonNVIDIAProber())
	if ran {
		t.Fatal("checkCmd must not run a command it could not find")
	}
	if !strings.Contains(report.String(), "dcgmi     not found") {
		t.Fatalf("doctor report lacks 'dcgmi     not found':\n%s", report.String())
	}
}

// A dcgmi binary that is on PATH but fails to run (e.g. the NVML driver
// library it needs is not loaded) reports "present (query failed)" rather
// than crashing or silently reporting nothing.
func TestDoctorReportsDCGMIPresentButQueryFailed(t *testing.T) {
	withDoctorExecSeams(t,
		func(name string) (string, error) {
			if name == "dcgmi" {
				return "/usr/bin/dcgmi", nil
			}
			return "", exec.ErrNotFound
		},
		func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("exit status 1") },
	)
	var report strings.Builder
	doctorWithProber(context.Background(), &report, doctorFixedNonNVIDIAProber())
	if !strings.Contains(report.String(), "dcgmi     present (query failed)") {
		t.Fatalf("doctor report lacks the dcgmi query-failed line:\n%s", report.String())
	}
}
