package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/version"
)

// Older installers handed the whole shared application root to the service
// account (owner gpuworks, mode 2775) when the server was installed directly in
// it. The current layout keeps that directory root's and shares only the
// server's own entries (see sharedApplicationRoot). A privileged install or
// upgrade migrates the older shape forward; nothing else may write to it.

// sharedRootMode is what the shared application root becomes: root-owned and
// writable by root only. It carries no setgid bit: the server's own entries get
// their group from chownTree, not from inheritance.
const sharedRootMode = 0o755

// Layout change kinds recorded in the install history.
const (
	// LayoutChangeSharedRootOwner is the shared application root moving from
	// the older installer's ownership to root's.
	LayoutChangeSharedRootOwner = "shared_root_owner"
	// LayoutChangeSharedConfigMoved is server.yaml moving beneath the private
	// server-config directory, behind the installer's compatibility alias.
	LayoutChangeSharedConfigMoved = "shared_config_moved"
)

// LayoutChange is one change an install or upgrade made to an installation's
// layout, recorded in the install history.
type LayoutChange struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
	// FromUID, FromGID and FromMode describe the path before a change of
	// ownership; ToUID, ToGID and ToMode after it. Mode is octal with the
	// setuid/setgid/sticky digit (2775).
	FromUID  *uint32 `json:"from_uid,omitempty"`
	FromGID  *uint32 `json:"from_gid,omitempty"`
	FromMode string  `json:"from_mode,omitempty"`
	ToUID    *uint32 `json:"to_uid,omitempty"`
	ToGID    *uint32 `json:"to_gid,omitempty"`
	ToMode   string  `json:"to_mode,omitempty"`
	// Target is where a moved path now lives.
	Target string `json:"target,omitempty"`
}

// legacySharedRootError is a shared application root in the older installer's
// shape: a real directory, not writable by others, but owned by an account
// other than root (or this operator) and/or writable by its group.
type legacySharedRootError struct {
	Dir    string
	UID    uint32
	GID    uint32
	Mode   os.FileMode
	Binary string
}

func (e *legacySharedRootError) Error() string {
	return fmt.Sprintf("shared application root %s still has an older installer's ownership (uid %d, gid %d, mode %s) and must become a root-owned directory that only root can write; "+
		"rerun this command with sudo to migrate it (for example `sudo %s upgrade`), or migrate it by hand with `sudo chown root:root %s && sudo chmod %04o %s`",
		e.Dir, e.UID, e.GID, octalMode(e.Mode), e.Binary, e.Dir, sharedRootMode, e.Dir)
}

func isLegacySharedRoot(err error) bool {
	var legacy *legacySharedRootError
	return errors.As(err, &legacy)
}

// legacySharedRoot classifies an untrusted shared root. Only the older
// installer's shape qualifies: a world-writable directory, a non-directory or a
// symlink stays a plain refusal.
func legacySharedRoot(dir string, info os.FileInfo) *legacySharedRootError {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o002 != 0 {
		return nil
	}
	return &legacySharedRootError{Dir: dir, UID: stat.Uid, GID: stat.Gid, Mode: info.Mode(), Binary: filepath.Join(dir, binaryName)}
}

// octalMode renders permission bits with the setuid/setgid/sticky digit.
func octalMode(mode os.FileMode) string {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return fmt.Sprintf("%04o", bits)
}

// migrateLegacySharedRoot moves a shared application root in the older
// installer's shape to root's ownership and mode 0755. It changes only that
// one directory: the server's entries inside it keep their owner, and
// chownTree later hands exactly those (and the data and log trees) to the
// service account, as for any shared-root installation. serviceUsers are the
// accounts allowed to own the older shape: the installed unit's and the one
// this run installs for.
//
// It runs only on mutating paths, before the strict layout checks; any other
// layout problem is left to those checks to report.
func (i *Installer) migrateLegacySharedRoot(ctx context.Context, serviceUsers ...string) error {
	_, err := i.layout.sharedConfig()
	var legacy *legacySharedRootError
	if !errors.As(err, &legacy) {
		return nil
	}
	if i.deps.Euid() != 0 && !i.deps.hasBinary("sudo") {
		return legacy
	}
	dir := legacy.Dir
	if !i.legacyRootOwner(legacy.UID, serviceUsers) {
		return fmt.Errorf("refusing to migrate the shared application root %s: it is owned by uid %d, which is neither root nor the service account; check who changed it, then fix it with `sudo chown root:root %s && sudo chmod %04o %s`",
			dir, legacy.UID, dir, sharedRootMode, dir)
	}
	// A parent another account can write could swap the directory for a
	// symlink between this check and the commands below.
	if _, err := trustedWriteParent(filepath.Dir(dir), 0); err != nil {
		return fmt.Errorf("refusing to migrate the shared application root %s: %w", dir, err)
	}
	before, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to migrate the shared application root %s: it is not a directory", dir)
	}
	// Extra ACL writers would survive the mode change; the operator removes
	// them first.
	if err := checkDirACL(dir); err != nil {
		return fmt.Errorf("refusing to migrate the shared application root %s: %w", dir, err)
	}
	// -h never follows a symlink; the parent check above keeps the directory
	// from being swapped for one before chmod.
	if err := i.deps.Run.Run(ctx, Command{Path: "chown", Args: []string{"-h", "0:0", dir}, Sudo: true}); err != nil {
		return fmt.Errorf("migrate the shared application root %s: %w", dir, err)
	}
	if err := i.deps.Run.Run(ctx, Command{Path: "chmod", Args: []string{fmt.Sprintf("%04o", sharedRootMode), dir}, Sudo: true}); err != nil {
		return fmt.Errorf("migrate the shared application root %s: %w", dir, err)
	}
	after, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("the shared application root %s changed during migration", dir)
	}
	if _, err := i.layout.sharedConfig(); err != nil {
		return fmt.Errorf("the shared application root %s is still not trusted after migration: %w", dir, err)
	}
	stat, _ := after.Sys().(*syscall.Stat_t)
	change := LayoutChange{Kind: LayoutChangeSharedRootOwner, Path: dir,
		FromUID: ptr(legacy.UID), FromGID: ptr(legacy.GID), FromMode: octalMode(legacy.Mode),
		ToUID: ptr(stat.Uid), ToGID: ptr(stat.Gid), ToMode: octalMode(after.Mode())}
	i.layoutChanges = append(i.layoutChanges, change)
	i.io.Say("migrated %s from uid %d gid %d mode %s to uid %d gid %d mode %s (older shared-root layout)",
		dir, legacy.UID, legacy.GID, change.FromMode, stat.Uid, stat.Gid, change.ToMode)
	return nil
}

func ptr(v uint32) *uint32 { return &v }

func checkDirACL(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	aclErr := securefile.CheckDirectoryACL(f)
	return errors.Join(aclErr, f.Close())
}

// legacyRootOwner reports whether uid may own the older shared-root shape:
// root, this operator, or a service account the installation names.
func (i *Installer) legacyRootOwner(uid uint32, serviceUsers []string) bool {
	if uid == 0 || int(uid) == os.Getuid() {
		return true
	}
	seen := map[string]bool{}
	for _, name := range append(serviceUsers, i.installedServiceUser()) {
		name = strings.TrimSpace(name)
		if name == "" || name == "root" || seen[name] {
			continue
		}
		seen[name] = true
		account, err := i.deps.LookupAccount(name)
		if err == nil && account.UID == strconv.FormatUint(uint64(uid), 10) {
			return true
		}
	}
	return false
}

// noteSharedConfigMove records isolateSharedConfig's move of server.yaml.
func (i *Installer) noteSharedConfigMove() {
	from, to := serverconfig.Path(i.layout.Dir), i.layout.sharedConfigFile()
	i.layoutChanges = append(i.layoutChanges, LayoutChange{Kind: LayoutChangeSharedConfigMoved, Path: from, Target: to})
	i.io.Say("moved %s to %s (it stays reachable at its old path)", from, to)
}

// recordLayoutChanges writes this run's layout changes to the install history,
// once. Like the other audit lines, a failure is a warning.
func (i *Installer) recordLayoutChanges(ctx context.Context, mode string) {
	if len(i.layoutChanges) == 0 {
		return
	}
	if err := i.createHistory(ctx, i.layout.HistoryPath(), 0o600); err != nil {
		i.io.Warn("warning: could not create the install history: %v", err)
	}
	if err := i.appendInstalled(ctx, i.layout.HistoryPath(), HistoryEntry{
		Action: ActionLayout, Result: "ok", ReleaseID: version.Version, Mode: mode,
		By: currentUserName(), At: stampNow(i.deps.now()), LayoutChanges: i.layoutChanges,
	}, 0o600); err != nil {
		i.io.Warn("warning: could not record the layout changes in the install history: %v", err)
		return
	}
	i.layoutChanges = nil
}
