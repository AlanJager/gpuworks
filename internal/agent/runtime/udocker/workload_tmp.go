package udocker

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// Guest /tmp gets its own short host directory. PRoot translates the guest
// /tmp onto the container's ROOT/tmp, a deep host path, and Unix domain
// sockets cap sun_path at 108 bytes: an engine that puts IPC sockets under
// /tmp (vLLM's ZMQ does, via tempfile) fails with ENAMETOOLONG once the
// translated path exceeds it.
//
// The directories live under <TMPDIR>/gpuworks-udocker-<euid>/<container>.
// The parent sits in a world-writable directory, so it is trusted only when
// it is a real directory owned by the agent and writable by nobody else;
// anything else (a pre-created directory or symlink) disables the bind rather
// than letting a root agent chown or chmod through a path another local user
// controls. Each container directory is owned by the workload identity
// (MountOwner) with mode 0700, so a dedicated run user can create its sockets
// and no one else can read or plant files there.

// workloadTmpRoot is the directory the per-agent parent is created in.
func (d *Driver) workloadTmpRoot() string {
	if d.hostTmpRoot != "" {
		return d.hostTmpRoot
	}
	return os.TempDir()
}

func (d *Driver) workloadTmpParent() string {
	return filepath.Join(d.workloadTmpRoot(), "gpuworks-udocker-"+strconv.Itoa(os.Geteuid()))
}

func (d *Driver) workloadTmpDir(name string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", fmt.Errorf("invalid container name %q", name)
	}
	return filepath.Join(d.workloadTmpParent(), name), nil
}

// ensureWorkloadTmp creates the host-side source of the guest /tmp bind. It
// runs on the launch path only; workloadMounts merely looks for its result.
func (d *Driver) ensureWorkloadTmp(name string) (string, error) {
	dir, err := d.workloadTmpDir(name)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(dir)
	if err := os.Mkdir(parent, 0o711); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if err := checkTrustedTmpParent(parent); err != nil {
		return "", err
	}
	// Traversable by the run user whatever the agent's umask.
	if err := os.Chmod(parent, 0o711); err != nil {
		return "", err
	}
	// The parent is agent-owned and not writable by others, so no one else can
	// swap the entry below between the Lstat and the chown/chmod.
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	if os.Geteuid() == 0 {
		owner := d.workloadTmpOwner()
		if err := os.Lchown(dir, owner.UID, owner.GID); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// workloadTmpOwner is the identity the workload runs as. Before provisioning
// has settled it, the agent's own identity is the only safe answer.
func (d *Driver) workloadTmpOwner() runtime.MountOwner {
	if owner, err := d.MountOwner(); err == nil {
		return owner
	}
	return runtime.MountOwner{UID: os.Geteuid(), GID: os.Getegid()}
}

// preparedWorkloadTmp reports the /tmp source ensureWorkloadTmp prepared for
// name, without creating anything.
func (d *Driver) preparedWorkloadTmp(name string) (string, bool) {
	dir, err := d.workloadTmpDir(name)
	if err != nil {
		return "", false
	}
	if checkTrustedTmpParent(filepath.Dir(dir)) != nil {
		return "", false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", false
	}
	return dir, true
}

// checkTrustedTmpParent accepts only a real directory owned by the agent that
// no other user can write to.
func checkTrustedTmpParent(parent string) error {
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", parent)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (%v)", parent, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not the agent (uid %d)", parent, st.Uid, os.Geteuid())
	}
	return nil
}

// removeWorkloadTmp reaps the container's short-path /tmp source. Best-effort
// by design: leftover IPC sockets never block a teardown.
func (d *Driver) removeWorkloadTmp(name string) {
	dir, err := d.workloadTmpDir(name)
	if err != nil || checkTrustedTmpParent(filepath.Dir(dir)) != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		slog.Warn("remove short-path tmp dir failed", "container", name, "err", err)
	}
}
