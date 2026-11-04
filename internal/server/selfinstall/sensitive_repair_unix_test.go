//go:build linux || darwin

package selfinstall

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// Root cannot observe ordinary permission denial. Run this same test as a
// numeric unprivileged UID in root CI, without creating an account or using
// external sudo. The copied test binary is the only public fixture.
func runSensitiveTestUnprivileged(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() != 0 {
		return false
	}
	dir, err := os.MkdirTemp("/tmp", "gpuworks-sensitive-regression-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "probe.test")
	if err := os.WriteFile(path, binary, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(path, "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), "TMPDIR=/tmp")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, Groups: []uint32{65534}}}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("unprivileged regression: %v\n%s", err, output)
	}
	return true
}

func TestSensitiveHardeningNeverUsesPrivilegedPathRepair(t *testing.T) {
	if runSensitiveTestUnprivileged(t) {
		return
	}
	h := newHarness(t, t.TempDir())
	secret := filepath.Join(h.layout.DataDir, "master.key")
	outside := filepath.Join(h.layout.Dir, "outside-secret")
	writeFile(t, secret, "existing encryption key", 0000)
	writeFile(t, outside, "external secret", 0600)
	if err := os.Chmod(secret, 0000); err != nil {
		t.Fatal(err)
	}
	policy, err := securefile.TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	swapped := false
	h.run.on = func(cmd Command) {
		if !swapped {
			swapped = true
			if err := os.Remove(secret); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, secret); err != nil {
				t.Fatal(err)
			}
		}
		// The old privileged runner follows the changed path just as chmod and
		// chgrp do. This models metadata damage to an outside file; the separate
		// review probe used a root-owned victim to confirm privilege escalation.
		switch cmd.Path {
		case "chmod":
			mode, e := strconv.ParseUint(cmd.Args[0], 8, 32)
			if e != nil {
				t.Fatal(e)
			}
			if err := os.Chmod(cmd.Args[1], os.FileMode(mode)); err != nil {
				t.Fatal(err)
			}
		case "chgrp":
			gid, e := strconv.Atoi(cmd.Args[0])
			if e != nil {
				t.Fatal(e)
			}
			if err := os.Chown(cmd.Args[1], -1, gid); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected privileged repair: %s", cmd.String())
		}
	}
	err = h.installer.hardenSensitiveFiles(context.Background(), policy)
	if err == nil {
		t.Fatal("permission failure was ignored")
	}
	info, statErr := os.Stat(outside)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("outside file modified before rejection: %#o", info.Mode().Perm())
	}
	if swapped || len(h.run.executed()) != 0 {
		t.Errorf("path-based privileged repair ran: %v", h.run.executed())
	}
	if !strings.Contains(err.Error(), "file owner or root") {
		t.Errorf("missing actionable safe-retry guidance: %v", err)
	}
}

func TestSensitivePermissionPreflightPreservesInstallationAndService(t *testing.T) {
	if runSensitiveTestUnprivileged(t) {
		return
	}
	for _, operation := range []string{"install", "upgrade"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t, t.TempDir())
			h.installSelf("new binary")
			h.writeExistingInstall("v1")
			if operation == "upgrade" {
				h.useSite("v2", []byte("replacement binary"))
			}
			key := filepath.Join(h.layout.DataDir, "master.key")
			keyBefore := readFile(t, key)
			if err := os.Chmod(key, 0000); err != nil {
				t.Fatal(err)
			}
			binaryBefore := readFile(t, h.layout.Binary())
			configBefore := readFile(t, h.layout.ConfigPath())
			var err error
			if operation == "install" {
				req := h.quiet()
				req.NoSystemd = true // changing supervisor used to stop first
				_, err = h.installer.Install(context.Background(), req)
			} else {
				_, err = h.installer.Upgrade(context.Background(), h.upgradeReq())
			}
			if err == nil {
				t.Fatal("unrepairable legacy key accepted")
			}
			if got := readFile(t, h.layout.Binary()); got != binaryBefore {
				t.Errorf("binary replaced before permission refusal: %q", got)
			}
			if got := readFile(t, h.layout.ConfigPath()); got != configBefore {
				t.Errorf("config replaced before permission refusal: %q", got)
			}
			for _, cmd := range h.run.executed() {
				if strings.Contains(cmd, " systemctl stop ") || strings.Contains(cmd, " systemctl restart ") || strings.Contains(cmd, " kill ") {
					t.Errorf("service changed before permission refusal: %s", cmd)
				}
			}
			info, err := os.Lstat(key)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("key changed identity: %v/%v", info, err)
			}
			if err := os.Chmod(key, 0600); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, key); got != keyBefore {
				t.Error("key bytes changed")
			}
		})
	}
}
