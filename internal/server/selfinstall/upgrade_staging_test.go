package selfinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// binaryAt records the installed binary's content when a command whose text
// starts with one of prefixes runs, keyed by the prefix (the last such run
// wins). The fake runner
// does not run chown or chmod, so what a test can prove is which file they
// named and what that path held at the time.
func binaryAt(h *harness, prefixes ...string) map[string]string {
	seen := map[string]string{}
	previous := h.run.on
	h.run.on = func(cmd Command) {
		if previous != nil {
			previous(cmd)
		}
		text := cmd.String()
		for _, prefix := range prefixes {
			if text == prefix || strings.HasPrefix(text, prefix+" ") {
				content, _ := os.ReadFile(h.layout.Binary())
				seen[prefix] = string(content)
			}
		}
	}
	return seen
}

// The binary an upgrade installs into the shared application root is root's
// and 0755 once it has its name, before the service starts: staged where the
// service account cannot reach it, made root's there, moved, and made root's
// again in place.
func TestSharedRootUpgradeInstallsTheNewBinaryAsRoots(t *testing.T) {
	for _, source := range []string{"site", "archive"} {
		t.Run(source, func(t *testing.T) {
			h, root := withLegacySharedRoot(t)
			req := UpgradeRequest{Keep: DefaultBackupKeep}
			if source == "site" {
				h.useSite("v2.0.0", []byte("new binary"))
				req.BaseURL = h.site.baseURL()
			} else {
				req.Archive = filepath.Join(t.TempDir(), "gpuworks.tar.gz")
				writeFile(t, req.Archive, string(tarGz(t, map[string]string{"gpuworks": "new binary"})), 0o644)
				staged := filepath.Join(root, upgradeStagingName, binaryName+".new")
				h.run.output[staged+" version"] = "gpuworks v2.0.0 (cafe1234, 2026-10-08T00:00:00Z)\n"
			}
			staging := filepath.Join(root, upgradeStagingName)
			staged := filepath.Join(staging, binaryName+".new")
			bin := h.layout.Binary()
			seen := binaryAt(h, "sudo chown -h 0:0 "+bin, "sudo systemctl restart")

			if _, err := h.installer.Upgrade(t.Context(), req); err != nil {
				t.Fatalf("upgrade: %v\n%s", err, h.errOut.String())
			}
			if got := readFile(t, bin); got != "new binary" {
				t.Fatalf("binary = %q", got)
			}
			// The staged file is made root's in the private directory, then
			// moved, then the installed file is made root's again — the last
			// chown of the binary before the service starts names the new
			// file, not the old one.
			if !h.run.ranInOrder("sudo systemctl stop", "sudo chown -h 0:0 "+staged, "sudo chmod 0755 "+staged,
				"sudo chown -h 0:0 "+bin, "sudo chmod 0755 "+bin, "sudo systemctl restart") {
				t.Fatalf("the new binary was not made root's before the service started: %v", h.run.executed())
			}
			lastChown, start := -1, -1
			for n, call := range h.run.executed() {
				switch {
				case call == "sudo chown -h 0:0 "+bin:
					lastChown = n
				case strings.HasPrefix(call, "sudo systemctl restart") && start < 0:
					start = n
				}
			}
			if lastChown < 0 || start < 0 || lastChown > start {
				t.Fatalf("no chown of the installed binary before the start: %v", h.run.executed())
			}
			if got := seen["sudo systemctl restart"]; got != "new binary" {
				t.Fatalf("binary at start = %q", got)
			}
			if got := seen["sudo chown -h 0:0 "+bin]; got != "new binary" {
				t.Fatalf("the last chown of %s saw %q, not the new binary", bin, got)
			}
			// Nothing of the new release went through the service account's
			// tmp/, and nothing is left behind.
			for _, call := range h.run.executed() {
				if strings.Contains(call, filepath.Join(h.layout.TmpDir(), binaryName)) {
					t.Fatalf("the new binary was staged in tmp/: %s", call)
				}
			}
			if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("staging directory left behind: %v", err)
			}
			info, err := os.Stat(bin)
			if err != nil || info.Mode().Perm() != binaryMode {
				t.Fatalf("binary mode = %v %v", info.Mode(), err)
			}
		})
	}
}

// A staged binary that changes between unpacking and the move is refused:
// the upgrade rolls back and the changed file never gets the binary's name.
func TestSharedRootUpgradeRefusesAStagedBinaryThatChanged(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, staging, staged string)
	}{
		{"rewritten", func(t *testing.T, _, staged string) {
			if err := os.WriteFile(staged, []byte("planted binary"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced", func(t *testing.T, _, staged string) {
			replacement := staged + ".planted"
			writeFile(t, replacement, "planted binary", 0o755)
			if err := os.Rename(replacement, staged); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory swapped", func(t *testing.T, staging, staged string) {
			if err := os.Rename(staging, staging+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(staging, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, staged, "planted binary", 0o755)
		}},
		{"directory opened up", func(t *testing.T, staging, _ string) {
			if err := os.Chmod(staging, 0o777); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, staging, staged string) {
			planted := filepath.Join(t.TempDir(), "planted")
			writeFile(t, planted, "new binary", 0o755)
			if err := os.Remove(staged); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(planted, staged); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, root := withLegacySharedRoot(t)
			h.useSite("v2.0.0", []byte("new binary"))
			staging := filepath.Join(root, upgradeStagingName)
			staged := filepath.Join(staging, binaryName+".new")
			previous := h.run.on
			h.run.on = func(cmd Command) {
				previous(cmd)
				if cmd.String() == "sudo systemctl stop "+ServiceName {
					tc.tamper(t, staging, staged)
				}
			}
			report, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
			if err == nil || !report.RolledBack || !strings.Contains(err.Error(), "refusing to install the staged binary") {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if got := readFile(t, h.layout.Binary()); got != "old binary" {
				t.Fatalf("binary = %q", got)
			}
			if h.run.ran("sudo chown -h 0:0 " + staged) {
				t.Fatalf("a changed staged binary was made root's: %v", h.run.executed())
			}
		})
	}
}

// A staging directory someone else left in the shared root is not reused.
func TestSharedRootUpgradeRefusesAForeignStagingDirectory(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	h.useSite("v2.0.0", []byte("new binary"))
	staging := filepath.Join(root, upgradeStagingName)
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeOwner(t, staging, 999, always)
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), staging+" is not a private directory of root") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop") {
		t.Fatal("the service was stopped")
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Fatalf("the foreign directory was removed: %v", err)
	}
}

// Outside the shared root the binary belongs to the service account with the
// rest of the tree, so the upgrade stages in tmp/ and moves it as before.
func TestDedicatedTreeUpgradeStagesInTmp(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	h.useSite("v2.0.0", []byte("new binary"))
	if _, err := h.installer.Upgrade(t.Context(), h.upgradeReq()); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary = %q", got)
	}
	if h.run.ran("sudo chown -h 0:0 " + h.layout.Binary()) {
		t.Fatalf("a dedicated tree's binary was made root's: %v", h.run.executed())
	}
	if _, err := os.Lstat(filepath.Join(h.layout.Dir, upgradeStagingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a private staging directory was created: %v", err)
	}
}
