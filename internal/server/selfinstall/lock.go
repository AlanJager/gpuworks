package selfinstall

import (
	"context"
	"fmt"
	"os"
	"strings"

	"gpuworks.ai/gpuworks/internal/launchd"
)

// lockMode is the mode the upgrade lock is kept at: group-writable, which is
// what every other file in the installation already is (dirMode 0o2775 on the
// directories, g+rwX from chownTree, 0o664 for server.yaml).
//
// It cannot stay 0600. The file outlives the command that creates it, and the
// two callers that take it are usually different accounts: the CLI runs as root
// (or as an operator with sudo) while the console upgrades from inside a server
// running as the service account. An account that cannot open the file cannot
// take the lock at all, and the group the installation already shares with that
// account is the mechanism meant to cover this — expose/lock.go reached the same
// conclusion for its own lock files and re-applies 0o664 for the same reason.
const lockMode = 0o664

// lockUpgrade takes the installation's upgrade lock and returns the release
// function. Two upgrades at once would interleave their stops, backups and
// binary swaps, so the second one fails immediately rather than waiting.
//
// Taking the lock also makes it usable by the account the service runs as, on
// every path that takes it — including `--check` and an installation already at
// the newest release, which return before the upgrade path's prepareDirs and
// chownTree would have normalized the tree.
func (i *Installer) lockUpgrade(ctx context.Context) (func(), error) {
	if err := mkdirAll(i.layout.Dir, dirMode); err != nil {
		return nil, err
	}
	path := i.layout.LockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, lockMode)
	if err != nil {
		// A lock file this account cannot open, as opposed to a directory it
		// cannot write: the installation predates this rule, so its lock is
		// 0600 and owned by whoever ran the CLI as root, while this run is the
		// service account the console upgrades as. Neither attribute can be
		// changed by an account that is neither the owner nor root, and a sudo
		// from here would be worse than useless — an in-service upgrade has no
		// terminal to answer a password prompt — so name the command that does
		// repair it. Any run as root repairs it on its own; see shareLock.
		if os.IsPermission(err) && fileExists(path) {
			return nil, fmt.Errorf("%w (ownership and mode of the existing lock are not openable by this account; run `sudo %s upgrade %s --check` once as root to repair it)",
				err, i.layout.Binary(), i.layout.Dir)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	i.shareLock(ctx, f, path)
	acquired, err := lockFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	if !acquired {
		f.Close()
		return nil, fmt.Errorf("another gpuworks upgrade is already running (lock %s)", path)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// shareLock leaves an open lock file usable by the account the service runs as.
// The lock is already open, so every failure here is narrated and not fatal:
// this run works either way, and the next one gets another chance.
func (i *Installer) shareLock(ctx context.Context, f *os.File, path string) {
	serviceUser := i.installedServiceUser()
	// Ownership. This is the second half, not the first: the installation
	// directory is setgid, so a file created here inherits its group no matter
	// which account creates it, and the service account's own group access
	// comes from that plus the group write below. chown only matters when the
	// directory's group is not the service account's — a tree root created
	// before the handover, or one an operator chowned by hand — and only root
	// can do anything about it. A non-root run cannot chown, and does not need
	// to: its file already carries whatever group the setgid directory gave it.
	if i.deps.Euid() == 0 && serviceUser != "" && serviceUser != "root" {
		group := i.serviceGroup(ctx, serviceUser)
		if err := i.deps.Run.Run(ctx, Command{Path: "chown", Args: []string{serviceUser + ":" + group, path}, Sudo: true}); err != nil {
			i.io.Warn("warning: could not give the upgrade lock %s to %s:%s: %v", path, serviceUser, group, err)
		}
	}
	// This half is the bug: the mode named in an open applies only to a file
	// that call creates, and the creating process's umask decides what lands on
	// disk. root's 022 turns 0664 into 0644, and the service account — which is
	// in the group but is not the owner — then cannot open the lock for writing,
	// which is what an exclusive flock needs. Re-apply it on every acquisition,
	// which repairs that and anything an earlier run stripped.
	//
	// The condition is the invariant itself — a group member can read and write
	// the file — and not "the mode differs from lockMode", because a file that
	// already admits the group needs no repair and is not this run's to
	// restyle. That matters because only the owner or root may chmod: a service
	// account opening a file root owns would otherwise warn on every console
	// upgrade, having opened the file perfectly well.
	if info, err := f.Stat(); err == nil && info.Mode().Perm()&0o660 != 0o660 {
		if err := f.Chmod(lockMode); err != nil {
			i.io.Warn("warning: could not widen the upgrade lock %s to %04o: %v", path, lockMode, err)
		}
	}
}

// installedServiceUser is the account the installation's service runs as, or ""
// when the installation has no separate account (root, or nothing installed
// yet). It reads the definition the installer wrote rather than asking the
// service manager, because the lock is taken before the service is consulted
// and on paths that never start it.
//
// The same guards readInstallation applies: a unit or plist counts only when it
// starts this layout's binary, so another installation on the same host — whose
// unit names another account — does not hand this lock to that account.
func (i *Installer) installedServiceUser() string {
	if unit, err := os.ReadFile(i.layout.UnitPath()); err == nil && unitRuns(unit, i.layout.Binary()) {
		if name := strings.TrimSpace(unitUser(unit)); name != "" {
			return name
		}
	}
	if plist, err := os.ReadFile(i.layout.LaunchdPath()); err == nil {
		if spec, err := launchd.ReadSpec(plist); err == nil && len(spec.Args) > 0 && spec.Args[0] == i.layout.Binary() {
			return strings.TrimSpace(spec.User)
		}
	}
	return ""
}
