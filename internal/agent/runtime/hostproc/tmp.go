package hostproc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Each instance's TMPDIR is a short path. Engines put Unix domain sockets
// there (vLLM's ZMQ IPC, through tempfile), and macOS caps a socket path at
// 104 bytes: under the agent's state directory the path is long enough that
// vLLM fails to start with "ipc path ... is longer than 103 characters".
//
// The directories are /tmp/gpuworks-host-<euid>/<hash of the instance>. The
// parent sits in world-writable /tmp, so it is used only when it is a real
// directory owned by the agent and writable by no one else; a pre-created
// directory or symlink of another user fails the launch instead of becoming
// a place the engine trusts.

func (d *Driver) tmpParent() string {
	return filepath.Join(d.tmpRoot, "gpuworks-host-"+strconv.Itoa(os.Geteuid()))
}

func (d *Driver) instanceTmp(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(d.tmpParent(), hex.EncodeToString(sum[:])[:12])
}

// prepareInstanceTmp creates the instance's TMPDIR, mode 0700.
func (d *Driver) prepareInstanceTmp(name string) (string, error) {
	parent := d.tmpParent()
	if err := os.Mkdir(parent, 0o700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("create %s: %w", parent, err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ok || int(stat.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s is not a private directory of this agent; remove it and retry", parent)
	}
	dir := d.instanceTmp(name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}
