package udocker

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

// runUserLockPath is the machine-wide lock around creating the runtime user.
// /run/lock is shared by every process on the host, unlike a service's
// private /tmp.
var runUserLockPath = func() string {
	if info, err := os.Stat("/run/lock"); err == nil && info.IsDir() {
		return "/run/lock/gpuworks-runtime-user.lock"
	}
	return filepath.Join(os.TempDir(), "gpuworks-runtime-user.lock")
}

// lockRunUserCreation serializes creating the runtime user across processes.
// Two `useradd -m` of one name at once -- `gpuworks-agent runveil` beside the
// agent on a fresh node, or parallel test binaries -- can leave the account
// and its home with different uids, a home the user can never enter. The
// returned func releases the lock; without a lock file it is a no-op.
func lockRunUserCreation() func() {
	file, err := os.OpenFile(runUserLockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		slog.Warn("runtime user creation is not serialized", "err", err)
		return func() {}
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
		slog.Warn("runtime user creation is not serialized", "err", err)
		return func() {}
	}
	return func() {
		_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
		_ = file.Close()
	}
}

// ensureRunUser creates name when it does not exist and looks it up. The
// lock makes a lookup that succeeds mean useradd has finished, home included.
func ensureRunUser(ctx context.Context, name string) (*user.User, error) {
	unlock := lockRunUserCreation()
	defer unlock()
	if account, err := user.Lookup(name); err == nil {
		return account, nil
	}
	slog.Info("runtime command", "runtime", "udocker", "args", []string{"useradd", "-m", name})
	_ = exec.CommandContext(ctx, "useradd", "-m", name).Run() // a concurrent creator outside our lock wins
	return user.Lookup(name)
}

// Seams for the run-user decision: the effective uid, the account lookup
// (which may create the account) and the ownership change. Tests replace them
// to decide as root without being root or creating accounts.
var (
	geteuid       = os.Geteuid
	lookupRunUser = ensureRunUser
	lchown        = os.Lchown
	createProbe   = os.CreateTemp
)

// dedicatedRunUser is the account a root agent runs workloads as: udocker
// refuses to run as root, and runveil keeps the same identity.
const dedicatedRunUser = "udocker"

// decideRunUser settles whom workloads run as. A non-root agent runs them as
// itself. A root agent prefers the dedicated user, and falls back to itself
// when that user cannot work here: the account is missing and cannot be
// created, the udocker CLI launcher cannot run Python as it, or the runtime's
// directories cannot be handed over to it. The fallback is a decision about
// the node, recorded for the doctor; a probe that could not run at all says
// nothing about the node and fails the setup instead, so a transient error
// (a full disk at restart) never moves workloads to root.
func (d *Driver) decideRunUser(ctx context.Context) error {
	if geteuid() != 0 {
		return nil
	}
	d.runUser = dedicatedRunUser
	reason := d.resolveRunUser(ctx)
	if reason == nil {
		for _, dir := range []string{d.udockerDir, d.stateDir} {
			refused, err := ownershipTransferable(dir, d.runUID, d.runGID)
			if err != nil {
				return err
			}
			if refused != nil {
				reason = refused
				break
			}
		}
	}
	if (d.runveil && reason == nil) || shouldUseDedicatedUdockerUser(d.pyBin, reason) {
		return nil
	}
	slog.Warn("dedicated runtime user is unusable; running as the agent's own user", "python", d.pyBin, "err", reason)
	d.track.setFallback(fallbackReason(reason))
	d.runUser, d.runUserHome = "", ""
	d.runUID, d.runGID = geteuid(), os.Getegid()
	return nil
}

// resolveRunUser looks up (creating it if need be) the dedicated account and,
// under the udocker CLI launcher, checks it can run Python. It returns why the
// account is unusable, nil when it is usable.
func (d *Driver) resolveRunUser(ctx context.Context) error {
	account, err := lookupRunUser(ctx, d.runUser)
	if err != nil {
		return err
	}
	d.runUserHome = account.HomeDir
	if d.runUID, err = strconv.Atoi(account.Uid); err != nil {
		return err
	}
	if d.runGID, err = strconv.Atoi(account.Gid); err != nil {
		return err
	}
	if d.runveil {
		return nil
	}
	// Only the udocker CLI launcher runs Python as this user.
	return exec.CommandContext(ctx, "runuser", "-u", d.runUser, "--", d.pyBin, "-c", "import sys").Run()
}

// ownershipTransferable reports whether a file created under dir can be
// handed to uid:gid. refused is why not, nil when it can: network filesystems
// may refuse that even to root (RunPod's NFSv4 /workspace answers EPERM). err
// is a probe that could not run, such as a full disk.
func ownershipTransferable(dir string, uid, gid int) (refused, err error) {
	probe, err := createProbe(dir, ".gpuworks-owner-probe-*")
	if err != nil {
		return nil, fmt.Errorf("probe file ownership under %s: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	defer os.Remove(name)
	if err := lchown(name, uid, gid); err != nil {
		return fmt.Errorf("%s refuses to change file ownership: %w", dir, err), nil
	}
	return nil, nil
}
