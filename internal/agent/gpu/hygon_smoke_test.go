package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestHygonSmokeVerdictGradesTheHostApartFromTheCard(t *testing.T) {
	exit := errors.New("exit status 1")
	for _, tc := range []struct {
		name, out  string
		err        error
		ok, onHost bool
	}{
		{"pass", "HYGON_HIP_SMOKE_OK\n", nil, true, false},
		{"pass with cleanup note", "HYGON_HIP_SMOKE_CLEANUP: hipFree=1\nHYGON_HIP_SMOKE_OK\n", nil, true, false},
		{"card failed", "HYGON_HIP_SMOKE_CARD_FAILED: hipMalloc=2\n", exit, false, false},
		{"runtime missing", "HYGON_HIP_SMOKE_UNAVAILABLE: hipInit=100\n", exit, false, true},
		{"python crashed", "Traceback (most recent call last):\n", exit, false, true},
		{"marker without success exit", "HYGON_HIP_SMOKE_OK\n", exit, false, true},
		{"no output", "", context.DeadlineExceeded, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := hygonSmokeVerdict([]byte(tc.out), tc.err)
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if got := HostRuntimeUnavailable(err) != ""; !tc.ok && got != tc.onHost {
				t.Fatalf("host-level = %v, want %v (err %v)", got, tc.onHost, err)
			}
		})
	}
}

func TestHygonSmokeEnvNamesTheCardAndDropsInheritedVisibility(t *testing.T) {
	env := hygonSmokeEnv([]string{"PATH=/bin", "HIP_VISIBLE_DEVICES=3", "ROCR_VISIBLE_DEVICES=1", "LD_LIBRARY_PATH=/tmp:"}, "/opt/dtk/lib/libamdhip64.so", "0000:67:00.0")
	joined := strings.Join(env, "\n")
	for _, want := range []string{"PATH=/bin", "HYGON_HIP_LIBRARY=/opt/dtk/lib/libamdhip64.so", "HYGON_SMOKE_BDF=0000:67:00.0", "LD_LIBRARY_PATH=/opt/dtk/lib:/opt/dtk/lib64:/opt/hyhal/lib"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("env lacks %q:\n%s", want, joined)
		}
	}
	for _, gone := range []string{"HIP_VISIBLE_DEVICES", "ROCR_VISIBLE_DEVICES", "/tmp"} {
		if strings.Contains(joined, gone) {
			t.Fatalf("env keeps %q:\n%s", gone, joined)
		}
	}
}

func TestHygonRuntimeSmokeWithoutDTKIsHostLevel(t *testing.T) {
	old := hygonSmokeLibraryFinder
	t.Cleanup(func() { hygonSmokeLibraryFinder = old })
	hygonSmokeLibraryFinder = func() (string, bool) { return "", false }
	if reason := HostRuntimeUnavailable(HygonRuntimeSmokeOn(t.Context(), "0000:67:00.0")); !strings.Contains(reason, "libamdhip64") {
		t.Fatalf("reason = %q", reason)
	}
	if _, err := exec.LookPath("python3"); err == nil {
		if reason := HygonHIPSmokePrerequisite(); !strings.Contains(reason, "DTK") {
			t.Fatalf("prerequisite = %q", reason)
		}
	}
	check, _ := HostRuntimeCheckFor(accelerator.VendorHygon)
	if v, ok := check.Visible(proto.GPUInfo{PCIeBDF: "0000:67:00.0"}); !ok || v != "0000:67:00.0" {
		t.Fatalf("Visible = %q %v", v, ok)
	}
	if _, ok := check.Visible(proto.GPUInfo{Index: 0}); ok {
		t.Fatal("a card without an address was named")
	}
}

// The script itself, run by python3 against a stand-in HIP library built
// from C: it finds the card by address however HIP prints it, grades where
// each failure happens, and never passes a card HIP does not list.
func TestHygonHIPSmokeScriptAgainstAStubRuntime(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed to run the check script")
	}
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("a C compiler is needed to build the stand-in HIP library")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "hip.c")
	stub := `#include <stdlib.h>
#include <string.h>
#include <stdio.h>
static int env(const char *n) { const char *v = getenv(n); return v ? atoi(v) : 0; }
int hipInit(unsigned f) { return env("STUB_INIT_RC"); }
int hipGetDeviceCount(int *n) { *n = 2; return 0; }
int hipDeviceGetPCIBusId(char *buf, int len, int dev) {
	snprintf(buf, len, dev == 0 ? "0000:08:00.0" : "0000:67:00.0"); return 0; }
int hipSetDevice(int dev) { return dev == 1 ? env("STUB_SET_RC") : 0; }
int hipMalloc(void **p, size_t n) { *p = malloc(n); return env("STUB_MALLOC_RC"); }
int hipFree(void *p) { free(p); return 0; }
`
	if err := os.WriteFile(source, []byte(stub), 0o644); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(dir, "libamdhip64.so")
	if out, err := exec.Command(compiler, "-shared", "-fPIC", "-o", library, source).CombinedOutput(); err != nil {
		t.Skipf("cannot build the stub: %v %s", err, out)
	}
	run := func(bdf string, extra ...string) error {
		cmd := exec.Command(python, "-c", accelerator.HygonHIPSmokeScript)
		cmd.Env = append(hygonSmokeEnv(os.Environ(), library, bdf), extra...)
		out, err := cmd.CombinedOutput()
		return hygonSmokeVerdict(out, err)
	}
	if err := run("00000000:67:00.0"); err != nil {
		t.Fatalf("healthy card: %v", err)
	}
	if err := run("0000:67:00.0", "STUB_MALLOC_RC=2"); err == nil || HostRuntimeUnavailable(err) != "" || !strings.Contains(err.Error(), "hipMalloc=2") {
		t.Fatalf("allocation failure: %v, want the card's verdict", err)
	}
	if err := run("0000:67:00.0", "STUB_SET_RC=101"); err == nil || HostRuntimeUnavailable(err) != "" {
		t.Fatalf("hipSetDevice failure: %v, want the card's verdict", err)
	}
	if reason := HostRuntimeUnavailable(run("0000:67:00.0", "STUB_INIT_RC=100")); !strings.Contains(reason, "hipInit=100") {
		t.Fatalf("hipInit failure: %q, want host-level", reason)
	}
	if reason := HostRuntimeUnavailable(run("0000:99:00.0")); !strings.Contains(reason, "none at 0000:99:00.0") {
		t.Fatalf("unlisted card: %q, want host-level", reason)
	}
}
