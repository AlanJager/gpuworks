package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
)

// An agent running in a container (a Kubernetes DaemonSet) can run the
// vendor's CLIs in the host's mount namespace instead of its own: they are
// the host's programs, linked against the host's C library and driver
// libraries, which a container of another distribution cannot load (the PPU
// SDK on Ubuntu 24.04 needs glibc 2.38; the agent image has 2.36). NVIDIA's
// runtime injects nvidia-smi into the container instead, so the switch is per
// vendor (accelerator.KubeProfile.HostCLIs) and set once at startup.

var cliOnHost atomic.Bool

// hostRoot is the host's root as an agent sharing its PID namespace sees it.
var hostRoot = "/proc/1/root"

// hostCLIDirs are searched for a CLI on the host: the usual PATH plus the
// vendor directories the drivers install their tools in.
var hostCLIDirs = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
	"/usr/local/Ascend/driver/tools", "/opt/hyhal/bin", "/opt/dtk/bin"}

// RunCLIsOnHost makes every vendor CLI the probes run execute in the host's
// mount namespace (the agent must share the host's PID namespace).
func RunCLIsOnHost() { cliOnHost.Store(true) }

// lookCLI finds a CLI where it will run: in PATH, or on the host.
func lookCLI(name string) (string, error) {
	if !cliOnHost.Load() {
		return exec.LookPath(name)
	}
	if filepath.IsAbs(name) {
		if fi, err := statOnHost(name); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return name, nil
		}
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	for _, dir := range hostCLIDirs {
		path := filepath.Join(dir, name)
		if fi, err := statOnHost(path); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return path, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// statOnHost stats a host path, following its symbolic links on the host:
// through /proc/1/root the kernel resolves an absolute link target against
// the agent's own root (Hygon's /usr/local/bin/hy-smi links to
// /usr/local/hyhal/bin/hy-smi, which the container does not have).
func statOnHost(path string) (os.FileInfo, error) {
	for i := 0; i < 16; i++ {
		fi, err := os.Lstat(filepath.Join(hostRoot, path))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			return fi, err
		}
		target, err := os.Readlink(filepath.Join(hostRoot, path))
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		path = filepath.Clean(target)
	}
	return nil, &os.PathError{Op: "stat", Path: path, Err: errors.New("too many levels of symbolic links")}
}

// cliCommand builds the command for a vendor CLI, on the host when CLIs run
// there.
func cliCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if !cliOnHost.Load() {
		return exec.CommandContext(ctx, name, args...)
	}
	path := name
	if found, err := lookCLI(name); err == nil {
		path = found
	}
	return exec.CommandContext(ctx, "nsenter", append([]string{"-t", "1", "-m", "--", path}, args...)...)
}
