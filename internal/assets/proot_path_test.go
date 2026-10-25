//go:build linux

package assets

import (
	"errors"
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
	t.Run("native", func(t *testing.T) {
		out, err := runGuestPathGate([]string{proot}, binary, source, guest, nil)
		if err != nil {
			t.Fatalf("embedded PRoot path gate: %v\n%s", err, out)
		}
		t.Log(strings.TrimSpace(string(out)))
	})
	testGuestPathGateSyscallAvailability(t, cc, dir, proot, binary, source, guest)
}

// Probe outside PRoot first. Only an actual host ENOSYS permits the narrower
// realpath check; ENOSYS introduced by PRoot on a capable host is a failure.
func runGuestPathGate(proot []string, binary, source, guest string, launcher []string) ([]byte, error) {
	run := func(argv ...string) ([]byte, error) {
		args := append(append([]string(nil), launcher...), argv...)
		return exec.Command(args[0], args[1:]...).CombinedOutput()
	}
	probe, err := run(binary, "--probe-faccessat2")
	var exit *exec.ExitError
	unavailable := errors.As(err, &exit) && exit.ExitCode() == 77
	if err != nil && !unavailable {
		return probe, fmt.Errorf("probe host faccessat2: %w", err)
	}
	args := append(append([]string(nil), proot...), "-0", "-r", "/", "-b", source+":"+guest, "-w", "/", binary, guest)
	if unavailable {
		args = append(args, "--realpath-only")
	}
	out, err := run(args...)
	if err != nil {
		return out, err
	}
	if !strings.Contains(string(out), "path_resolution=pass") {
		return out, fmt.Errorf("missing path gate success marker")
	}
	return out, nil
}
