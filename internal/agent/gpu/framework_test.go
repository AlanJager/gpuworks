package gpu

import "testing"

func TestParseFrameworkVersion(t *testing.T) {
	if got := parseFrameworkVersion("Driver Version: 550.90\nCUDA Version: 12.8\n", `(?im)CUDA Version\s*:\s*([0-9]+(?:\.[0-9]+){0,2})`); got != "12.8" {
		t.Fatalf("cuda version = %q", got)
	}
	if got := parseCANNFrameworkVersion("CANN Version: 8.5.0\n"); got != "8.5.0" {
		t.Fatalf("cann version = %q", got)
	}
	if got := parseCANNFrameworkVersion("npu-smi 26.0.rc1\n"); got != "" {
		t.Fatalf("driver-only CANN version = %q", got)
	}
}
