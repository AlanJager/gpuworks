package assets

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// Compile the actual probe and a HIP fixture, then check quota enforcement,
// over-allocation, visible-device count, and missing memory symbols.
func TestPartitionProbeHygonMemoryQuota(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the partition probe runs on Linux")
	}
	for _, program := range []string{"cc", "bash"} {
		if _, err := exec.LookPath(program); err != nil {
			if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
				t.Fatal(err)
			}
			t.Skipf("%s unavailable", program)
		}
	}
	cmd := exec.Command("bash", "../../build/runtime-assets/probe/test-probe.sh")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("HIP quota probe: %v\n%s", err, out)
	}
}
