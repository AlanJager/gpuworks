package accelerator

import "testing"

// Which runtimes need the vendor's container plugin. Getting this wrong in
// either direction is costly: demanding a plugin that is not needed takes a
// working node out of service, and not demanding one that is leaves every
// deployment on the node failing at container start.
func TestRuntimeNeedsGPUPluginPerVendorAndRuntime(t *testing.T) {
	for _, tc := range []struct {
		vendor, driver string
		want           bool
		why            string
	}{
		{VendorNVIDIA, "docker", true, "docker goes through the NVIDIA Container Toolkit"},
		{VendorNVIDIA, "nerdctl", true, "nerdctl's --gpus needs the toolkit too"},
		{VendorNVIDIA, "udocker", false, "udocker binds the device nodes itself"},
		{VendorAscend, "docker", true, "docker needs the Ascend Docker Runtime"},
		{VendorAscend, "nerdctl", false, "nerdctl is given the devices and driver mounts directly"},
		{VendorAscend, "udocker", false, "udocker binds the device nodes itself"},
		{VendorHygon, "docker", false, "device passthrough the agent arranges"},
		{VendorPPU, "docker", false, "device passthrough the agent arranges"},
		{"", "docker", false, "a node with no accelerator needs no plugin"},
		{VendorNVIDIA, "", false, "no runtime selected yet"},
	} {
		if got := RuntimeNeedsGPUPlugin(tc.vendor, tc.driver); got != tc.want {
			t.Errorf("%s + %s = %v, want %v (%s)", tc.vendor, tc.driver, got, tc.want, tc.why)
		}
	}
	// The vendor name is normalized like everywhere else.
	if !RuntimeNeedsGPUPlugin("NVIDIA", "Docker") {
		t.Error("vendor and runtime names must be matched case-insensitively")
	}
}

// Passing a runtime over is only safe where runveil can serve the cards:
// on an Ascend host it would trade a clear "plugin missing" for a runtime
// that fails CANN at start.
func TestOnlyNVIDIANodesFallBackToRunveil(t *testing.T) {
	for vendor, want := range map[string]bool{
		"nvidia": true, " NVIDIA ": true,
		"ascend": false, "hygon": false, "ppu": false, "apple": false, "": false,
	} {
		if got := RuntimeGPUFallback(vendor); got != want {
			t.Errorf("%q = %v, want %v", vendor, got, want)
		}
	}
}
