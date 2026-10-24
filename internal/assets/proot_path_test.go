//go:build linux

package assets

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the embedded binary, rather than just checking its syscall table: libc
// realpath checks and explicit faccessat2 calls must use the guest namespace.
func TestEmbeddedProotResolvesGuestAccessPaths(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal("C compiler required for PRoot path gate")
		}
		t.Skip("C compiler unavailable")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.MkdirAll(filepath.Join(source, "data", "leaf"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("data", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "gate")
	if out, err := exec.Command(cc, "-O2", "-Wall", "-Wextra", "-Werror", "../../build/runtime-assets/proot/gate_path_resolution.c", "-o", binary).CombinedOutput(); err != nil {
		t.Fatalf("compile gate: %v\n%s", err, out)
	}
	proot, err := ExtractProot(ProotStaticNoFD, filepath.Join(dir, "proot"))
	if err != nil {
		t.Fatal(err)
	}
	guest := fmt.Sprintf("/gpuworks-path-gate-%d", os.Getpid())
	if _, err := os.Lstat(guest); !os.IsNotExist(err) {
		t.Fatalf("guest path exists on host or cannot be checked: %v", err)
	}
	out, err := exec.Command(proot, "-0", "-r", "/", "-b", source+":"+guest, "-w", "/", binary, guest).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "path_resolution=pass") {
		t.Fatalf("embedded PRoot path gate: %v\n%s", err, out)
	}
}
