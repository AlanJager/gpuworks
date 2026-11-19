package selfinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ownedInfo reports a path as owned by another uid (and optionally another
// inode), which the suite cannot create without root.
type ownedInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (o ownedInfo) Sys() any { return &o.stat }

// fakeOwner makes lstatOwned report path as owned by uid while owned()
// is true.
func fakeOwner(t *testing.T, path string, uid uint32, owned func() bool) {
	t.Helper()
	previous := lstatOwned
	t.Cleanup(func() { lstatOwned = previous })
	lstatOwned = func(name string) (os.FileInfo, error) {
		info, err := previous(name)
		if err != nil || filepath.Clean(name) != filepath.Clean(path) || !owned() {
			return info, err
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Uid = uid
		return ownedInfo{FileInfo: info, stat: stat}, nil
	}
}

func always() bool { return true }

// assertUntouched checks that a refused run left the older layout alone: no
// migration, no handoff, no stopped service, the old binary in place.
func assertUntouched(t *testing.T, h *harness, root string) {
	t.Helper()
	if h.run.ran("sudo chown") || h.run.ran("sudo systemctl stop") {
		t.Fatalf("refused run changed the host: %v", h.run.executed())
	}
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "2775" {
		t.Fatalf("root mode = %s", octalMode(info.Mode()))
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatalf("binary = %q", got)
	}
}

func TestLegacySharedRootOwnedByAnUnrelatedAccountIsRefused(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	fakeOwner(t, root, 4242, always)
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "owned by uid 4242, which is neither root nor the service account") ||
		!strings.Contains(err.Error(), "sudo chown root:root "+root) {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, h, root)
}

func TestLegacySharedRootOwnedByTheServiceAccountMigrates(t *testing.T) {
	for _, euid := range []int{0, 1000} {
		t.Run(map[int]string{0: "root", 1000: "sudo"}[euid], func(t *testing.T) {
			h, root := withLegacySharedRoot(t)
			h.installer.deps.Euid = func() int { return euid }
			h.binaries["sudo"] = euid != 0
			chowned := false
			chmod := h.run.on
			h.run.on = func(cmd Command) {
				chmod(cmd)
				if cmd.Path == "chown" && len(cmd.Args) == 3 && cmd.Args[2] == root {
					chowned = true
				}
			}
			fakeOwner(t, root, 4242, func() bool { return !chowned })
			h.installer.deps.LookupAccount = func(name string) (Account, error) {
				if name == DefaultServiceUser {
					return Account{UID: "4242"}, nil
				}
				return Account{UID: "9999"}, nil
			}
			if err := h.installer.migrateLegacySharedRoot(t.Context(), DefaultServiceUser); err != nil {
				t.Fatal(err)
			}
			if !h.run.ranInOrder("sudo chown -h 0:0 "+root, "sudo chmod 00755 "+root) {
				t.Fatalf("commands = %v", h.run.executed())
			}
			if c := h.installer.layoutChanges[0]; c.Kind != LayoutChangeSharedRootOwner || *c.FromUID != 4242 {
				t.Fatalf("change = %+v", c)
			}
		})
	}
}

func TestLegacySharedRootSwappedDuringMigrationIsRefused(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	chmodded := false
	chmod := h.run.on
	h.run.on = func(cmd Command) {
		chmod(cmd)
		if cmd.Path == "chmod" && len(cmd.Args) == 2 && cmd.Args[1] == root {
			chmodded = true
		}
	}
	previous := lstatOwned
	t.Cleanup(func() { lstatOwned = previous })
	lstatOwned = func(name string) (os.FileInfo, error) {
		info, err := previous(name)
		if err != nil || name != root || !chmodded {
			return info, err
		}
		stat := *info.Sys().(*syscall.Stat_t)
		stat.Ino++
		return ownedInfo{FileInfo: info, stat: stat}, nil
	}
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "changed during migration") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop") || h.run.ran("sudo chown -R") {
		t.Fatalf("a swapped root was used: %v", h.run.executed())
	}
}

func TestLegacySharedRootWithAnExtraACLIsRefused(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	previous := checkDirACL
	t.Cleanup(func() { checkDirACL = previous })
	checkDirACL = func(dir string) error {
		if dir == root {
			return errors.New("directory has extended ACL entries")
		}
		return previous(dir)
	}
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "refusing to migrate the shared application root") ||
		!strings.Contains(err.Error(), "extended ACL") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, h, root)
}

func TestLegacySharedRootUnderAnUntrustedParentIsRefused(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	parent := filepath.Dir(root)
	if err := os.Chmod(parent, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "untrusted writer can replace privileged path "+parent) {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, h, root)
}

// A chmod that fails after chown leaves a root-owned, still group-writable
// root: the older shape, which the next run migrates.
func TestLegacySharedRootMigrationFailingHalfwayIsFinishedByARerun(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	chmod := h.run.on
	failing := true
	h.run.on = func(cmd Command) {
		if !failing {
			chmod(cmd)
		}
	}
	h.run.fail["sudo chmod 00755 "+root] = errors.New("chmod: interrupted")
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "still writable by its group, rerun the command to finish") {
		t.Fatalf("err = %v", err)
	}
	if h.run.ran("sudo systemctl stop") || readFile(t, h.layout.Binary()) != "old binary" {
		t.Fatalf("the failed migration went on: %v", h.run.executed())
	}
	failing = false
	delete(h.run.fail, "sudo chmod 00755 "+root)
	if _, err := h.installer.Upgrade(t.Context(), h.upgradeReq()); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "0755" {
		t.Fatalf("root mode after rerun = %s", octalMode(info.Mode()))
	}
	if got := readFile(t, h.layout.Binary()); got != "new binary" {
		t.Fatalf("binary after rerun = %q", got)
	}
}

// Before the migration the service account owned the root and could replace
// any entry; root must not follow what it planted.
func TestLegacySharedRootRefusesEntriesTheServiceAccountCouldHavePlanted(t *testing.T) {
	cases := map[string]struct {
		plant func(t *testing.T, h *harness, root string)
		want  string
	}{
		"symlinked data": {
			plant: func(t *testing.T, h *harness, root string) {
				outside := t.TempDir()
				if err := os.RemoveAll(h.layout.DataDir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, h.layout.DataDir); err != nil {
					t.Fatal(err)
				}
				fakeOwner(t, h.layout.DataDir, 4242, always)
			},
			want: "is a symlink that root did not create",
		},
		"symlinked server.yaml": {
			plant: func(t *testing.T, h *harness, root string) {
				outside := filepath.Join(t.TempDir(), "secret")
				writeFile(t, outside, readFile(t, h.layout.ConfigPath()), 0o600)
				if err := os.Remove(h.layout.ConfigPath()); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, h.layout.ConfigPath()); err != nil {
					t.Fatal(err)
				}
			},
			want: "is not the installer's canonical alias",
		},
		"hard-linked server.yaml": {
			plant: func(t *testing.T, h *harness, root string) {
				if err := os.Link(h.layout.ConfigPath(), filepath.Join(filepath.Dir(root), "linked")); err != nil {
					t.Fatal(err)
				}
			},
			want: "has 2 hard links",
		},
		"hard-linked binary": {
			plant: func(t *testing.T, h *harness, root string) {
				if err := os.Link(h.layout.Binary(), filepath.Join(filepath.Dir(root), "linked")); err != nil {
					t.Fatal(err)
				}
			},
			want: "has 2 hard links",
		},
		"backups replaced by a file": {
			plant: func(t *testing.T, h *harness, root string) {
				if err := os.RemoveAll(h.layout.BackupsDir); err != nil {
					t.Fatal(err)
				}
				writeFile(t, h.layout.BackupsDir, "", 0o644)
			},
			want: "is not a directory",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h, root := withLegacySharedRoot(t)
			tc.plant(t, h, root)
			h.useSite("v2.0.0", []byte("new binary"))
			_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if h.run.ran("sudo chown -R") || h.run.ran("sudo chmod -R") || h.run.ran("sudo systemctl stop") {
				t.Fatalf("root acted on a planted entry: %v", h.run.executed())
			}
			if fileExists(h.layout.sharedConfigFile()) {
				t.Fatal("the configuration was copied from a planted entry")
			}
		})
	}
}

// A data directory that is the operator's own link to another disk stays
// allowed in the shared root, as it was before.
func TestSharedRootDataLinkCreatedByTheOperatorIsAllowed(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	outside := filepath.Join(t.TempDir(), "data")
	if err := os.Rename(h.layout.DataDir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, h.layout.DataDir); err != nil {
		t.Fatal(err)
	}
	h.useSite("v2.0.0", []byte("new binary"))
	if _, err := h.installer.Upgrade(t.Context(), h.upgradeReq()); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "0755" {
		t.Fatalf("root mode = %s", octalMode(info.Mode()))
	}
}

// After migration a refusal says the root was migrated instead of claiming
// nothing changed.
func TestRefusalAfterMigrationSaysTheRootWasMigrated(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	h.useSite("v2.0.0", []byte("new binary"))
	if err := os.Remove(filepath.Join(h.layout.DataDir, "master.key")); err != nil {
		t.Fatal(err)
	}
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil {
		t.Fatal("upgrade went on without the master key")
	}
	want := "only the shared application root " + root + " was migrated to root ownership; nothing else was changed"
	if !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "; nothing was changed") {
		t.Fatalf("err = %v, want %q", err, want)
	}
}
