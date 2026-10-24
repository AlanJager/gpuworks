package assets

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise the actual C probe with a DSMI fixture: physical IDs and logical
// IDs are distinct, absent devices return 87, and unsliced memory is 32 GiB.
func TestPartitionProbeAscendDeviceIdentity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the partition probe runs on Linux")
	}
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("C compiler required to test the embedded probe source")
		}
		t.Skip("C compiler unavailable")
	}
	dir := t.TempDir()
	fixture := filepath.Join(dir, "dsmi.c")
	const source = `
#include <stdlib.h>
struct hbm { unsigned long long size; unsigned int freq; unsigned long long usage; int temp; unsigned int bandwidth; };
#ifndef OMIT_MAPPING
int dsmi_get_logicid_from_phyid(unsigned int physical, unsigned int *logical) {
    if (physical != 6 || getenv("MAP_FAIL")) return 87;
    if (getenv("MAP_EMPTY")) return 0;
    *logical = getenv("LOGICAL_ID") ? atoi(getenv("LOGICAL_ID")) : 6;
    return 0;
}
#endif
int dsmi_get_hbm_info(int logical, struct hbm *info) {
    int expected = getenv("LOGICAL_ID") ? atoi(getenv("LOGICAL_ID")) : 6;
    if (logical != expected || getenv("HBM_FAIL")) return 87;
    info->size = getenv("UNSLICED") ? 33554432ULL : 4194304ULL;
    return 0;
}
`
	if err := os.WriteFile(fixture, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	compile := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
			t.Fatalf("compile: %v\n%s", err, out)
		}
	}
	probe := filepath.Join(dir, "probe")
	lib := filepath.Join(dir, "dsmi.so")
	missingLib := filepath.Join(dir, "dsmi-no-mapping.so")
	compile("-Wall", "-Wextra", "-Werror", "../../build/runtime-assets/probe/probe.c", "-ldl", "-o", probe)
	compile("-shared", "-fPIC", fixture, "-o", lib)
	compile("-shared", "-fPIC", "-DOMIT_MAPPING", fixture, "-o", missingLib)
	for _, tc := range []struct {
		name, device, extra, want string
		code                      int
	}{
		{"host IDs retained", "6", "", "physical_id=6 logical_id=6 rc=0 total_kb=4194304", 0},
		{"OCI IDs compacted", "6", "LOGICAL_ID=0", "physical_id=6 logical_id=0 rc=0 total_kb=4194304", 0},
		{"sparse host compacted", "6", "LOGICAL_ID=1", "physical_id=6 logical_id=1 rc=0 total_kb=4194304", 0},
		{"absent physical device", "0", "", "physical_id=0 rc=87", 4},
		{"mapping error", "6", "MAP_FAIL=1", "physical_id=6 rc=87", 4},
		{"mapping without an ID", "6", "MAP_EMPTY=1", "physical_id=6 rc=0 logical_id=4294967295", 4},
		{"missing mapping symbol", "6", "NO_MAPPING=1", "dsmi_get_logicid_from_phyid unavailable", 3},
		{"memory query error", "6", "HBM_FAIL=1", "logical_id=6 rc=87 total_kb=0", 4},
		{"quota not applied", "6", "UNSLICED=1", "total_kb=33554432", 4},
		{"unset device", "unset", "", "invalid GPUWORKS_PARTITION_PROBE_DEVICE", 2},
		{"empty device", "", "", "invalid GPUWORKS_PARTITION_PROBE_DEVICE", 2},
		{"negative device", "-1", "", "invalid GPUWORKS_PARTITION_PROBE_DEVICE", 2},
		{"partial device", "6x", "", "invalid GPUWORKS_PARTITION_PROBE_DEVICE", 2},
		{"overflow device", "4294967296", "", "invalid GPUWORKS_PARTITION_PROBE_DEVICE", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			library := lib
			if tc.extra == "NO_MAPPING=1" {
				library = missingLib
			}
			cmd := exec.Command(probe)
			cmd.Env = []string{"LD_PRELOAD=" + library, "GPUWORKS_PARTITION_PROBE_BACKEND=ascend-dgpu", "GPUWORKS_PARTITION_PROBE_QUOTA_MIB=4096"}
			if tc.device != "unset" {
				cmd.Env = append(cmd.Env, "GPUWORKS_PARTITION_PROBE_DEVICE="+tc.device)
			}
			if tc.extra != "" {
				cmd.Env = append(cmd.Env, tc.extra)
			}
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code || !strings.Contains(string(out), tc.want) {
				t.Fatalf("exit=%v err=%v output=%s; want code=%d and %q", cmd.ProcessState, err, out, tc.code, tc.want)
			}
		})
	}
}
