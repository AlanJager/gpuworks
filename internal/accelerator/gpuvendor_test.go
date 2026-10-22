package accelerator

import "testing"

func TestGPUVendorFallsBackToTheFrameworkOnlyForAnEmptyRow(t *testing.T) {
	cases := []struct {
		name                                   string
		vendor, gpu, arch, cc, framework, want string
	}{
		{name: "reported vendor wins", vendor: "Huawei", framework: "cuda", want: VendorAscend},
		{name: "name identifies the card", gpu: "Ascend 910B4", framework: "", want: VendorAscend},
		{name: "empty row takes the framework", framework: "cann", want: VendorAscend},
		{name: "empty row, unknown framework", framework: "", want: VendorNVIDIA},
		// An architecture alone is enough to stop the framework fallback: it
		// was reported by the card itself.
		{name: "arch only", arch: "Ampere", framework: "cann", want: VendorNVIDIA},
		{name: "compute capability only", cc: "8.9", framework: "cann", want: VendorAscend},
	}
	for _, c := range cases {
		if got := GPUVendor(c.vendor, c.gpu, c.arch, c.cc, c.framework); got != c.want {
			t.Errorf("%s: GPUVendor = %q, want %q", c.name, got, c.want)
		}
	}
}
