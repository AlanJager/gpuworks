package selfinstall

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// lockFixture is an installation whose unit names the service account, which is
// what tells the lock whose group it belongs to.
func lockFixture(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, t.TempDir())
	h.writeExistingInstall("v1.0.0")
	if got := h.installer.installedServiceUser(); got != DefaultServiceUser {
		t.Fatalf("installed service user = %q, want %q", got, DefaultServiceUser)
	}
	return h
}

// lockStat reports the mode and owner of the lock file as it really is on disk.
func lockStat(t *testing.T, h *harness) os.FileMode {
	t.Helper()
	info, err := os.Stat(h.layout.LockPath())
	if err != nil {
		t.Fatalf("stat lock: %v", err)
	}
	return info.Mode().Perm()
}

// TestUpgradeLockIsOpenableByTheServiceAccount covers the reported bug from both
// directions: a lock the CLI left behind must not shut the console out, and the
// lock must not be readable by accounts outside the installation either.
func TestUpgradeLockIsOpenableByTheServiceAccount(t *testing.T) {
	for _, tc := range []struct {
		name string
		// euid is the account taking the lock: 0 is the CLI run as root, which
		// is how the field breakage happened.
		euid int
		// existing is the mode of a lock file left by an earlier run, 0 for a
		// lock file that does not exist yet.
		existing os.FileMode
		// wantSudoChown is whether taking the lock has to hand the file over.
		wantSudoChown bool
	}{
		{name: "root creates the lock", euid: 0, wantSudoChown: true},
		{name: "the service account creates the lock", euid: 999},
		{name: "root repairs a lock an older release left 0600", euid: 0, existing: 0o600, wantSudoChown: true},
		{name: "the service account repairs its own stripped mode", euid: 999, existing: 0o600},
		{name: "root widens a lock left without group write", euid: 0, existing: 0o640, wantSudoChown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := lockFixture(t)
			h.deps.Euid = func() int { return tc.euid }
			h.installer = New(h.layout, h.deps, h.io)
			if tc.existing != 0 {
				writeFile(t, h.layout.LockPath(), "", tc.existing)
			}

			unlock, err := h.installer.lockUpgrade(context.Background())
			if err != nil {
				t.Fatalf("take the lock: %v", err)
			}
			defer unlock()

			if got := lockStat(t, h); got != lockMode {
				t.Fatalf("lock mode = %04o, want %04o: an account outside the owner and the "+
					"installation's group cannot open it, which is the reported failure", got, lockMode)
			}
			// The group half is what the service account actually relies on, so
			// assert it is not merely "wider than 0600".
			if lockMode&0o070 == 0 {
				t.Fatalf("lock mode %04o grants the group nothing", lockMode)
			}
			chown := "sudo chown " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.LockPath()
			if ran := h.run.ran(chown); ran != tc.wantSudoChown {
				t.Fatalf("ran %q = %v, want %v; commands: %v", chown, ran, tc.wantSudoChown, h.run.executed())
			}
		})
	}
}

// TestUpgradeCheckLeavesALockTheConsoleCanTake is the reported sequence: a
// root-run check, then the console upgrade. Both paths take the same lock, and
// the check returns long before the upgrade path's chownTree would have fixed
// the tree.
func TestUpgradeCheckLeavesALockTheConsoleCanTake(t *testing.T) {
	h := lockFixture(t)
	h.useSite("v2.0.0", []byte("new binary"))
	h.deps.Euid = func() int { return 0 }
	h.installer = New(h.layout, h.deps, h.io)

	req := h.upgradeReq()
	req.Check = true
	report, err := h.installer.Upgrade(context.Background(), req)
	if err != nil {
		t.Fatalf("check: %v (stderr: %s)", err, h.errOut.String())
	}
	if !report.Checked || report.UpToDate {
		t.Fatalf("check report = %+v", report)
	}
	if got := lockStat(t, h); got != lockMode {
		t.Fatalf("a root-run --check left the lock %04o, want %04o", got, lockMode)
	}

	// The console upgrades from inside the server, as the service account. It
	// has to be able to open and lock the file the CLI just wrote.
	h.deps.Euid = func() int { return 999 }
	h.installer = New(h.layout, h.deps, h.io)
	unlock, err := h.installer.lockUpgrade(context.Background())
	if err != nil {
		t.Fatalf("the service account could not take the lock the CLI left: %v", err)
	}
	unlock()
}

// TestUpgradeUpToDateLeavesALockTheConsoleCanTake is the other early return: the
// installation is already at the release the channel offers, so the upgrade path
// never runs.
func TestUpgradeUpToDateLeavesALockTheConsoleCanTake(t *testing.T) {
	h := lockFixture(t)
	h.useSite("v1.0.0", []byte("new binary"))
	h.deps.Euid = func() int { return 0 }
	h.installer = New(h.layout, h.deps, h.io)

	report, err := h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err != nil {
		t.Fatalf("upgrade: %v (stderr: %s)", err, h.errOut.String())
	}
	if !report.UpToDate {
		t.Fatalf("report = %+v", report)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("an up-to-date installation was stopped: %v", h.run.executed())
	}
	if got := lockStat(t, h); got != lockMode {
		t.Fatalf("an up-to-date check left the lock %04o, want %04o", got, lockMode)
	}
}

// TestStageInServiceSharesTheUpgradeLock covers the console path: the server
// takes the same lock from inside a process that already runs as the service
// account, so it must not hand the file to itself or widen the mode.
func TestStageInServiceSharesTheUpgradeLock(t *testing.T) {
	for _, tc := range []struct {
		name          string
		euid          int
		wantSudoChown bool
	}{
		{name: "the server runs as the service account", euid: 999},
		{name: "the server runs as root", euid: 0, wantSudoChown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := lockFixture(t)
			h.deps.Euid = func() int { return tc.euid }
			h.installer = New(h.layout, h.deps, h.io)
			archive := h.writeArchive("v2.0.0", []byte("new binary"))

			if _, err := h.installer.StageInService(context.Background(), "v1.0.0",
				openArchive(t, archive), writeSnapshot("live snapshot"), "admin"); err != nil {
				t.Fatalf("stage: %v", err)
			}
			if got := lockStat(t, h); got != lockMode {
				t.Fatalf("lock mode after an in-service stage = %04o, want %04o", got, lockMode)
			}
			chown := "sudo chown " + DefaultServiceUser + ":" + DefaultServiceUser + " " + h.layout.LockPath()
			if ran := h.run.ran(chown); ran != tc.wantSudoChown {
				t.Fatalf("ran %q = %v, want %v", chown, ran, tc.wantSudoChown)
			}
		})
	}
}

// TestUpgradeLockNamesTheRepairForAFileItCannotOpen: an installation from a
// release that created the lock 0600 root-owned cannot be repaired by the
// account that cannot open it, and a sudo from inside the server would have no
// terminal to answer. The error has to name the command that does work.
func TestUpgradeLockNamesTheRepairForAFileItCannotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root bypasses mode 0000. Run this exact assertion as an account
		// without DAC privileges, including in root-run CI containers. Copy
		// the binary out of go test's private build directory so that account
		// can execute it; only its own fixture directory needs to be writable.
		dir, err := os.MkdirTemp("", "gpuworks-lock-denied-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		source, err := os.Open(executable)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		childBinary := filepath.Join(dir, "selfinstall.test")
		target, err := os.OpenFile(childBinary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(target, source)
		modeErr := target.Chmod(0o755)
		closeErr := target.Close()
		if modeErr != nil {
			t.Fatal(modeErr)
		}
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		work := filepath.Join(dir, "work")
		if err := os.Mkdir(work, 0o700); err != nil {
			t.Fatal(err)
		}
		const deniedID = 65534
		if err := os.Chown(work, deniedID, deniedID); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(childBinary, "-test.run=^TestUpgradeLockNamesTheRepairForAFileItCannotOpen$", "-test.v")
		cmd.Dir = work
		cmd.Env = append(os.Environ(), "TMPDIR="+work)
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: deniedID, Gid: deniedID}}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("permission assertion as unprivileged account: %v\n%s", err, output)
		}
		return
	}
	h := lockFixture(t)
	writeFile(t, h.layout.LockPath(), "", 0o000)
	if _, err := os.Stat(h.layout.LockPath()); err != nil {
		t.Fatalf("stat: %v", err)
	}

	_, err := h.installer.lockUpgrade(context.Background())
	if err == nil {
		t.Fatal("a lock this account cannot open was reported as taken")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v, want the underlying permission failure", err)
	}
	if !strings.Contains(err.Error(), "sudo") || !strings.Contains(err.Error(), "--check") {
		t.Fatalf("err = %v, want it to name the command that repairs the lock", err)
	}
}

// TestUpgradeLockKeepsExclusivity: sharing the file must not weaken the
// exclusion it exists for.
func TestUpgradeLockKeepsExclusivity(t *testing.T) {
	h := lockFixture(t)
	h.useSite("v2.0.0", []byte("new binary"))
	unlock, err := h.installer.lockUpgrade(context.Background())
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer unlock()

	_, err = h.installer.Upgrade(context.Background(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("err = %v, want a held-lock refusal", err)
	}
	if h.run.ran("sudo systemctl stop gpuworks") {
		t.Fatalf("the refused upgrade touched the service: %v", h.run.executed())
	}
}

// TestInstalledServiceUserReadsTheServiceDefinition keeps the lock's idea of the
// account in step with the definition the installer writes.
func TestInstalledServiceUserReadsTheServiceDefinition(t *testing.T) {
	h := newHarness(t, t.TempDir())
	if got := h.installer.installedServiceUser(); got != "" {
		t.Fatalf("service user before install = %q, want none", got)
	}
	h.writeExistingInstall("v1.0.0")
	if got := h.installer.installedServiceUser(); got != DefaultServiceUser {
		t.Fatalf("service user = %q, want %q", got, DefaultServiceUser)
	}
	// A root-run unit names no separate account, and the lock then stays the
	// operator's.
	writeFile(t, h.layout.UnitPath(), RenderUnit(UnitConfig{
		Binary: h.layout.Binary(), ConfigPath: h.layout.ConfigPath(), DataDir: h.layout.DataDir, ServiceUser: "root",
	}), 0o644)
	if got := h.installer.installedServiceUser(); got != "root" {
		t.Fatalf("service user = %q, want root", got)
	}
}

// TestUpgradeLockDefeatsTheCreatingUmask is the mechanism the field bug got
// wrong: the mode an open names is filtered by the creating process's umask, so
// root's usual 022 turns even a shared mode into one the service account cannot
// write. Taking the lock has to leave the file usable regardless.
func TestUpgradeLockDefeatsTheCreatingUmask(t *testing.T) {
	// TestMain does not restore the umask, so put it back before returning.
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	h := lockFixture(t)
	h.deps.Euid = func() int { return 0 }
	h.installer = New(h.layout, h.deps, h.io)

	// What an open alone would produce, to prove the test is actually running
	// under a stripping umask.
	probe := filepath.Join(h.layout.Dir, "umask-probe")
	raw, err := os.OpenFile(probe, os.O_CREATE|os.O_RDWR, lockMode)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	info, err := raw.Stat()
	raw.Close()
	if err != nil {
		t.Fatalf("stat probe: %v", err)
	}
	if got := info.Mode().Perm(); got == lockMode {
		t.Fatalf("the umask did not strip the probe (mode %04o); the test proves nothing", got)
	}
	if got := info.Mode().Perm(); got&0o020 != 0 {
		t.Fatalf("the probe kept group write (mode %04o), so this umask does not reproduce the field failure", got)
	}

	unlock, err := h.installer.lockUpgrade(context.Background())
	if err != nil {
		t.Fatalf("take the lock: %v", err)
	}
	defer unlock()
	if got := lockStat(t, h); got != lockMode {
		t.Fatalf("lock mode = %04o, want %04o despite umask 022", got, lockMode)
	}
}

// TestUpgradeLockDoesNotRestyleAFileThatAlreadyAdmitsTheGroup: a lock root owns
// with the group bits in place is one the service account can use as it stands,
// and only the owner or root may chmod it — so the console upgrade must open it
// and be quiet, not warn about a mode it was never able to change.
func TestUpgradeLockDoesNotRestyleAFileThatAlreadyAdmitsTheGroup(t *testing.T) {
	h := lockFixture(t)
	writeFile(t, h.layout.LockPath(), "", lockMode|0o400)
	h.deps.Euid = func() int { return 999 }
	h.installer = New(h.layout, h.deps, h.io)

	unlock, err := h.installer.lockUpgrade(context.Background())
	if err != nil {
		t.Fatalf("take a group-writable lock: %v", err)
	}
	defer unlock()

	if warned := h.errOut.String(); strings.Contains(warned, "upgrade lock") {
		t.Fatalf("a usable lock was restyled: %q", warned)
	}
	if ran := h.run.ran("sudo chmod"); ran {
		t.Fatalf("chmod ran for a lock that already admits the group: %v", h.run.executed())
	}
}

// TestInstalledServiceUserIgnoresAnotherInstallationsUnit: two installations on
// one host each have their own unit, and only the one that starts this layout's
// binary may decide whose group this lock joins.
func TestInstalledServiceUserIgnoresAnotherInstallationsUnit(t *testing.T) {
	h := newHarness(t, t.TempDir())
	writeFile(t, h.layout.UnitPath(), RenderUnit(UnitConfig{
		Binary:      "/opt/other/server/gpuworks",
		ConfigPath:  "/opt/other/server/server.yaml",
		DataDir:     "/opt/other/data",
		ServiceUser: "someoneelse",
	}), 0o644)
	if got := h.installer.installedServiceUser(); got != "" {
		t.Fatalf("service user from another installation's unit = %q, want none", got)
	}
}
