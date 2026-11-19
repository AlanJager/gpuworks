package selfinstall

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// withLegacySharedRoot lays down an installation as older installers left it
// in the shared application root: the root itself and the server's
// directories group-writable and setgid (2775), the binary 0775 and server.yaml
// 0664 directly in the root. Ownership by the service account cannot be
// modeled without root; a group-writable root is the same shape to the check.
func withLegacySharedRoot(t *testing.T) (*harness, string) {
	t.Helper()
	root, _ := withSharedRoot(t)
	h := newHarness(t, root)
	h.writeExistingInstall("v1.0.0")
	for _, dir := range []string{h.layout.DataDir, h.layout.LogsDir, h.layout.BackupsDir, h.layout.TmpDir()} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, os.ModeSetgid|0o775); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(h.layout.Binary(), 0o775); err != nil {
		t.Fatal(err)
	}
	writeFile(t, h.layout.LockPath(), "", 0o660)
	if err := os.Chmod(root, os.ModeSetgid|0o775); err != nil {
		t.Fatal(err)
	}
	// The fake runner records commands without running them; apply the
	// migration's chmod for real so the layout checks that follow see it.
	// chown cannot be applied without root, and the suite's own account is
	// already trusted.
	h.run.on = func(cmd Command) {
		if cmd.Path == "chmod" && len(cmd.Args) == 2 && cmd.Args[1] == root {
			if err := gnuChmod(root, cmd.Args[0]); err != nil {
				t.Error(err)
			}
		}
	}
	return h, root
}

// gnuChmod applies a numeric chmod the way GNU chmod does: on a directory a
// mode of four digits or fewer keeps the setuid and setgid bits, and only a
// fifth (leading) digit clears them.
func gnuChmod(path, arg string) error {
	bits, err := strconv.ParseUint(arg, 8, 32)
	if err != nil {
		return err
	}
	mode := os.FileMode(bits & 0o777)
	if bits&0o4000 != 0 {
		mode |= os.ModeSetuid
	}
	if bits&0o2000 != 0 {
		mode |= os.ModeSetgid
	}
	if bits&0o1000 != 0 {
		mode |= os.ModeSticky
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.IsDir() && len(arg) <= 4 {
		mode |= info.Mode() & (os.ModeSetuid | os.ModeSetgid)
	}
	return os.Chmod(path, mode)
}

func layoutHistory(t *testing.T, h *harness) []LayoutChange {
	t.Helper()
	entries, err := ReadHistory(h.layout.HistoryPath())
	if err != nil {
		t.Fatal(err)
	}
	var changes []LayoutChange
	for _, entry := range entries {
		if entry.Action == ActionLayout {
			if entry.Result != "ok" {
				t.Fatalf("layout entry result = %q", entry.Result)
			}
			changes = append(changes, entry.LayoutChanges...)
		}
	}
	return changes
}

func TestLegacySharedRootUpgradeMigratesAndRollbackKeepsTheTrustedRoot(t *testing.T) {
	for _, failHealth := range []bool{false, true} {
		t.Run(map[bool]string{false: "upgrade", true: "rollback"}[failHealth], func(t *testing.T) {
			h, root := withLegacySharedRoot(t)
			agentEntries := []string{"bin", "partition", "probe-libs", "runveil", "models", "identity.json"}
			notRootOwned(t, root, agentEntries)
			h.useSite("v2.0.0", []byte("new binary"))
			if failHealth {
				h.healthErr = errors.New("new version unhealthy")
			}
			report, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
			if (err != nil) != failHealth || report.RolledBack != failHealth {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if !h.run.ranInOrder("sudo chown -h 0:0 "+root, "sudo chmod 00755 "+root, "sudo systemctl stop") {
				t.Fatalf("migration did not run before the service stopped: %v", h.run.executed())
			}
			info, err := os.Lstat(root)
			if err != nil {
				t.Fatal(err)
			}
			if got := octalMode(info.Mode()); got != "0755" {
				t.Fatalf("shared root mode = %s, want 0755", got)
			}
			// The service-writable entries keep their own modes; chownTree
			// hands exactly these to the service account.
			for _, dir := range []string{h.layout.DataDir, h.layout.LogsDir, h.layout.BackupsDir, h.layout.TmpDir()} {
				if info, err := os.Stat(dir); err != nil || info.Mode().Perm()&0o020 == 0 {
					t.Fatalf("%s lost group write: %v %v", dir, info.Mode(), err)
				}
			}
			for _, entry := range []string{h.layout.sharedConfigDir(), h.layout.LockPath(), h.layout.TmpDir(), h.layout.BackupsDir} {
				if !h.run.ran("sudo chown -R " + DefaultServiceUser + ":" + DefaultCredentialGroup + " " + entry) {
					t.Fatalf("%s was not handed to the service account: %v", entry, h.run.executed())
				}
			}
			// Root runs the binary (sudo upgrade, root supervisor hooks), so
			// it stays root's: never handed over, made root's after the
			// handoff and again after a rollback restores it.
			bin := h.layout.Binary()
			if h.run.ran("sudo chown -R " + DefaultServiceUser + ":" + DefaultCredentialGroup + " " + bin) {
				t.Fatalf("the binary was handed to the service account: %v", h.run.executed())
			}
			if !h.run.ranInOrder("sudo chown -R "+DefaultServiceUser+":"+DefaultCredentialGroup+" "+h.layout.TmpDir(),
				"sudo chown -h 0:0 "+bin, "sudo chmod 0755 "+bin, "sudo systemctl stop") {
				t.Fatalf("the binary was not made root's after the handoff: %v", h.run.executed())
			}
			if failHealth && !h.run.ranInOrder("sudo systemctl stop", "sudo chown -h 0:0 "+bin) {
				t.Fatalf("rollback did not make the restored binary root's: %v", h.run.executed())
			}
			if h.run.ran("sudo chown -R " + DefaultServiceUser + ":" + DefaultCredentialGroup + " " + root) {
				t.Fatal("the shared root itself was handed back to the service account")
			}
			if path := h.layout.ConfigPath(); path != h.layout.sharedConfigFile() {
				t.Fatalf("config = %s, want the private path", path)
			}
			changes, foreign := splitForeign(layoutHistory(t, h))
			if len(changes) != 2 || changes[0].Kind != LayoutChangeSharedRootOwner || changes[1].Kind != LayoutChangeSharedConfigMoved {
				t.Fatalf("layout history = %+v", changes)
			}
			// The agent's files from withSharedRoot do not belong to root
			// (notRootOwned): kept, warned about and recorded.
			for _, name := range agentEntries {
				path := filepath.Join(root, name)
				if c, ok := foreign[path]; !ok || c.FromUID == nil || c.FromMode == "" {
					t.Fatalf("foreign entry %s not recorded: %+v", path, foreign)
				}
				if !strings.Contains(h.errOut.String(), path+" is owned by uid ") {
					t.Fatalf("no warning names %s:\n%s", path, h.errOut.String())
				}
			}
			if len(foreign) != 6 {
				t.Fatalf("server entries recorded as foreign: %+v", foreign)
			}
			if c := changes[0]; c.Path != root || c.FromMode != "2775" || c.ToMode != "0755" || c.FromUID == nil || c.ToUID == nil {
				t.Fatalf("root change = %+v", c)
			}
			if c := changes[1]; c.Path != filepath.Join(root, "server.yaml") || c.Target != h.layout.sharedConfigFile() {
				t.Fatalf("config change = %+v", c)
			}
			out := h.out.String()
			if !strings.Contains(out, "migrated "+root+" from uid ") || !strings.Contains(out, "moved "+filepath.Join(root, "server.yaml")+" to ") {
				t.Fatalf("output does not name each change:\n%s", out)
			}
			if failHealth {
				if got := readFile(t, h.layout.Binary()); got != "old binary" {
					t.Fatalf("rollback binary = %q", got)
				}
				// The previous release runs in the migrated layout as it does
				// after the configuration move: the root is never handed back.
				if !strings.Contains(h.unitFile(), "--config "+h.layout.sharedConfigFile()+"\n") {
					t.Fatal("rollback lost the private configuration path")
				}
			}
			// The migration is done once; the next run sees a trusted root.
			if _, err := h.layout.sharedConfig(); err != nil {
				t.Fatalf("layout after migration: %v", err)
			}
		})
	}
}

// notRootOwned makes sure the named entries of root are not owned by uid 0,
// as the node agent's files in a shared root are not. They are created by the
// suite's own account, which is already the case unless the suite runs as
// root (CI does); root hands them to an unprivileged uid.
func notRootOwned(t *testing.T, root string, names []string) {
	t.Helper()
	if os.Getuid() != 0 {
		return
	}
	const unprivileged = 65534
	for _, name := range names {
		if err := os.Lchown(filepath.Join(root, name), unprivileged, unprivileged); err != nil {
			t.Fatal(err)
		}
	}
}

// splitForeign separates kept foreign entries from the other layout changes.
func splitForeign(all []LayoutChange) ([]LayoutChange, map[string]LayoutChange) {
	var changes []LayoutChange
	foreign := map[string]LayoutChange{}
	for _, c := range all {
		if c.Kind == LayoutChangeForeignEntryKept {
			foreign[c.Path] = c
		} else {
			changes = append(changes, c)
		}
	}
	return changes, foreign
}

func TestLegacySharedRootReadOnlyUpgradeDoesNotMigrate(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	h.useSite("v2.0.0", []byte("new"))
	req := h.upgradeReq()
	req.Check = true
	report, err := h.installer.Upgrade(t.Context(), req)
	if err != nil || !report.Checked {
		t.Fatalf("check on the older layout: report=%+v err=%v", report, err)
	}
	if h.run.ran("sudo chown -h") || h.run.ran("sudo chmod") {
		t.Fatalf("--check changed the layout: %v", h.run.executed())
	}
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "2775" {
		t.Fatalf("root mode = %s", octalMode(info.Mode()))
	}
}

func TestLegacySharedRootWithoutPrivilegeRefusesWithTheCommand(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	h.binaries["sudo"] = false
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil {
		t.Fatal("unprivileged upgrade migrated the older layout")
	}
	for _, want := range []string{
		"shared application root " + root + " still has an older installer's ownership",
		"mode 2775",
		"`sudo " + filepath.Join(root, "gpuworks") + " upgrade`",
		"`sudo chown root:root " + root + " && sudo chmod 00755 " + root + "`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if got := readFile(t, h.layout.Binary()); got != "old binary" {
		t.Fatal("refused upgrade replaced the binary")
	}
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "2775" {
		t.Fatal("refused upgrade changed the root")
	}
	if h.run.ran("sudo systemctl stop") {
		t.Fatal("refused upgrade stopped the service")
	}
}

func TestLegacySharedRootInstallRepairMigrates(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	h.users[DefaultServiceUser] = true
	h.installer.WithSelf(h.installSelf("new binary"))
	req := h.quiet()
	h.mustInstall(req)
	if info, _ := os.Lstat(root); octalMode(info.Mode()) != "0755" {
		t.Fatalf("root mode = %s", octalMode(info.Mode()))
	}
	if changes, _ := splitForeign(layoutHistory(t, h)); len(changes) != 2 || changes[0].Kind != LayoutChangeSharedRootOwner {
		t.Fatalf("layout history = %+v", changes)
	}
}

func TestWorldWritableSharedRootIsNotMigrated(t *testing.T) {
	h, root := withLegacySharedRoot(t)
	if err := os.Chmod(root, 0o777); err != nil {
		t.Fatal(err)
	}
	h.useSite("v2.0.0", []byte("new binary"))
	_, err := h.installer.Upgrade(t.Context(), h.upgradeReq())
	if err == nil || !strings.Contains(err.Error(), "must remain a trusted non-writable directory") {
		t.Fatalf("err = %v, want the plain refusal", err)
	}
	if h.run.ran("sudo chown -h") {
		t.Fatal("a world-writable root was migrated")
	}
}

func TestLegacySharedRootOwnerMustBeRootOrTheServiceAccount(t *testing.T) {
	h := newHarness(t, t.TempDir())
	h.installer.deps.LookupAccount = func(name string) (Account, error) {
		if name == "svc" {
			return Account{UID: "4321"}, nil
		}
		return Account{UID: "9999"}, nil
	}
	if !h.installer.legacyRootOwner(0, nil) || !h.installer.legacyRootOwner(uint32(os.Getuid()), nil) {
		t.Fatal("root or this operator rejected")
	}
	if !h.installer.legacyRootOwner(4321, []string{"svc"}) {
		t.Fatal("the service account rejected")
	}
	if h.installer.legacyRootOwner(4321, []string{"other", "root", ""}) {
		t.Fatal("an unrelated account accepted")
	}
}
