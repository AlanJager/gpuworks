package udocker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

// withoutPython hides every python3 the driver could find, or skips when a
// fixed fallback location outside PATH still provides one.
func withoutPython(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	if findPython3() != "" {
		t.Skip("a python3 outside PATH (Conda location) cannot be hidden on this host")
	}
}

func TestRunveilDetectsWithoutPython(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("udocker driver is Linux-only")
	}
	withoutPython(t)
	t.Setenv(launcherEnv, "")
	if ok, err := New(t.TempDir()).Detect(context.Background()); err != nil || !ok {
		t.Fatalf("runveil must not require python: ok=%v err=%v", ok, err)
	}
	t.Setenv(launcherEnv, "udocker")
	if ok, _ := New(t.TempDir()).Detect(context.Background()); ok {
		t.Fatal("the udocker CLI launcher was detected without python")
	}
}

func TestRunveilProvisioningNeedsOnlyProot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root provisioning creates a system user")
	}
	withoutPython(t)
	t.Setenv(launcherEnv, "")
	base := t.TempDir()
	d := New(base)
	if err := d.ensureProvisioned(context.Background()); err != nil {
		t.Fatalf("provisioning without python: %v", err)
	}
	info, err := os.Stat(d.prootPath)
	if err != nil || info.Mode()&0o111 == 0 || info.Size() < 1<<20 {
		t.Fatalf("patched proot not extracted at %q: %v", d.prootPath, err)
	}
	if _, err := os.Stat(filepath.Join(base, "offline", "udocker")); !os.IsNotExist(err) {
		t.Fatalf("the udocker bundle was extracted although nothing needed it: %v", err)
	}
	// A CLI call explains why it cannot run instead of failing obscurely.
	_, err = d.udo(context.Background(), "images")
	if err == nil || !strings.Contains(err.Error(), "udocker CLI unavailable: python3") {
		t.Fatalf("CLI call without python: %v", err)
	}
}

func TestRunveilProvisionsTheCLIOnFirstUse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root provisioning creates a system user")
	}
	if findPython3() == "" {
		t.Skip("python3 is required to run the udocker CLI")
	}
	t.Setenv(launcherEnv, "")
	base := t.TempDir()
	makeUdockerTempRootTraversable(t, base)
	d := New(base)
	if err := d.ensureProvisioned(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d.cliConfigured() {
		t.Fatal("the CLI was provisioned eagerly")
	}
	out, err := d.udo(context.Background(), "--version")
	if err != nil || !strings.Contains(out, "udocker") {
		t.Fatalf("lazy CLI provisioning: %q %v", out, err)
	}
	if !d.cliConfigured() {
		t.Fatal("CLI not recorded after provisioning")
	}
}

// Provisioning the udocker CLI while a workload is executing bin/proot-* used
// to rewrite that binary in place from the bundle's englib and fail with
// ETXTBSY (an agent restarted next to running containers), or, when nothing
// ran, briefly install an unpatched PRoot. The bundle's proot builds are now
// skipped and the patched one is only ever replaced by rename.
func TestProvisionCLIDoesNotRewriteARunningProot(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("ETXTBSY is Linux behavior")
	}
	archName, ok := engineArchName(goruntime.GOARCH)
	if !ok {
		t.Skip("no embedded proot for this architecture")
	}
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	base := t.TempDir()
	d := New(base)
	running := filepath.Join(d.udockerDir, "bin", "proot-"+archName)
	data, err := os.ReadFile(sleepBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(running), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(running, data, 0o755); err != nil {
		t.Fatal(err)
	}
	busy := exec.Command(running, "30")
	if err := busy.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = busy.Process.Kill(); _ = busy.Wait() })

	bundle := filepath.Join(base, "bundle")
	for path, body := range map[string]string{
		"englib/bin/proot-" + archName:            "unpatched upstream proot",
		"englib/bin/proot-" + archName + "-4_8_0": "unpatched upstream proot",
		"englib/lib/libfake.so":                   "englib content",
		"bin/udocker":                             "#!/bin/sh\necho udocker 1.3.17\n",
	} {
		full := filepath.Join(bundle, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	d.bundleDir = bundle
	d.embeddedBundle = false
	if ok, err := d.useOfflineBundle(context.Background()); err != nil || !ok {
		t.Fatalf("provisioning next to a running proot: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(d.udockerDir, "lib", "libfake.so")); err != nil {
		t.Fatalf("englib was not copied: %v", err)
	}
	for _, name := range []string{"proot-" + archName, "proot-" + archName + "-4_8_0"} {
		got, err := os.ReadFile(filepath.Join(d.udockerDir, "bin", name))
		if err != nil || strings.Contains(string(got), "unpatched") || len(got) < 1<<20 {
			t.Fatalf("%s is not the embedded patched proot: len=%d err=%v", name, len(got), err)
		}
	}
}
