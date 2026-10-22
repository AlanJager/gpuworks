//go:build multiproc

package l2

import (
	"os"
	"os/exec"
	"testing"
)

func TestMultiprocessToolchainAvailable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatalf("go toolchain unavailable in strict mode: %v", err)
		}
		t.Skipf("go unavailable: %v", err)
	}
}
