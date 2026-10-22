package accelerator

import (
	"testing"
)

func TestCheckDriverCompatCUDAHasNoUpperBound(t *testing.T) {
	for _, tc := range []struct {
		driver, framework, version string
		want                       CompatLevel
	}{
		{"450.80.02", "cuda", "11.8", ""},
		{"550.54.15", "cuda", "11.8", ""},
		{"580.10", "cuda", "13.0", ""},
		{"500.00", "cuda", "12.0", CompatWarning},
	} {
		got, _ := CheckDriverCompat(tc.driver, tc.framework, tc.version)
		if got != tc.want {
			t.Fatalf("CheckDriverCompat(%q,%q,%q) = %q, want %q", tc.driver, tc.framework, tc.version, got, tc.want)
		}
	}
}

func TestDriverRequirementForCUDA(t *testing.T) {
	if got := DriverRequirement("cuda", "13.0"); got != "CUDA driver >= 580" {
		t.Fatalf("CUDA 13 driver requirement = %q", got)
	}
	if got := DriverRequirement("cuda", "12.9"); got != "CUDA driver >= 525" {
		t.Fatalf("CUDA 12 driver requirement = %q", got)
	}
	for _, tc := range []struct {
		framework string
		version   string
		want      string
	}{
		{"cann", "9.1", "Ascend HDK 25.2 - 26.1"},
		{"cann", "8.5", "Ascend HDK 25.0 - 26.1"},
		{"dtk", "24.04", "DTK driver 5.7.1-V1.0 - 5.7.1-V1.6.3"},
		{"hggc", "13.0", "KMD >= 1.6"},
	} {
		if got := DriverRequirement(tc.framework, tc.version); got != tc.want {
			t.Fatalf("%s/%s driver requirement = %q, want %q", tc.framework, tc.version, got, tc.want)
		}
	}
}

func TestCheckDriverCompatNormalizesAscendAndDTK(t *testing.T) {
	if got, _ := CheckDriverCompat("npu-smi 26.0.rc1 Version: 26.0.rc1", "cann", "9.1.0"); got != "" {
		t.Fatalf("CANN 9.1 with HDK 26.0.rc1 = %q, want compatible", got)
	}
	if got, _ := CheckDriverCompat("rock-5.7.1-V1.6.8", "dtk", "24.04"); got != CompatWarning {
		t.Fatalf("DTK upper-bound warning = %q, want warning", got)
	}
}

func TestNormalizeDriverVersionRemovesToolHeaderDuplication(t *testing.T) {
	if got := NormalizeDriverVersion("ascend", "npu-smi 26.0.rc1 Version: 26.0.rc1"); got != "26.0.RC1" {
		t.Fatalf("ascend driver display = %q", got)
	}
	if got := NormalizeDriverVersion("nvidia", "NVIDIA-SMI 570.153.02 Driver Version: 570.153.02 CUDA Version: 12.8"); got != "570.153.02" {
		t.Fatalf("nvidia driver display = %q", got)
	}
}

func TestCheckDriverCompatHGGCStaysUnknownWithoutTuple(t *testing.T) {
	got, _ := CheckDriverCompat("1.6.0", "hggc", "13.0")
	if got != CompatUnknown {
		t.Fatalf("HGGC result = %q, want unknown", got)
	}
}

// A host environment package records the oldest macOS it was built for; the
// node's driver is its macOS version.
func TestCheckDriverCompatMetalComparesMacOSVersions(t *testing.T) {
	for _, tc := range []struct {
		driver, min string
		want        CompatLevel
	}{
		{"15.4.1", "15.0", ""},
		{"15.0", "15.0", ""},
		{"26.2", "15.0", ""},
		{"14.7.2", "15.0", CompatWarning},
		{"15.4.1", "26.2", CompatWarning},
		{"", "15.0", CompatUnknown},
		{"15.4.1", "", CompatUnknown},
		{"unknown", "15.0", CompatUnknown},
	} {
		if got, _ := CheckDriverCompat(tc.driver, "metal", tc.min); got != tc.want {
			t.Errorf("CheckDriverCompat(%q, metal, %q) = %q, want %q", tc.driver, tc.min, got, tc.want)
		}
	}
	if got := DriverRequirement("metal", "15.0"); got != "macOS >= 15.0" {
		t.Fatalf("DriverRequirement(metal) = %q", got)
	}
}
