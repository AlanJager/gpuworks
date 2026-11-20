package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

func validateClusterCheckNodeDetailed(ctx context.Context, req clusterCheckRequest, native bool, processSupport func() error, checks map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Reject malformed requests before reporting any host prerequisite as checked.
	if err := validateClusterCheckRequest(req); err != nil {
		return err
	}
	check := func(name string, run func() error) error {
		err := run()
		if checks != nil {
			checks[name] = ""
			if err != nil {
				checks[name] = err.Error()
			}
		}
		return err
	}
	if err := check("runtime", func() error { return validateClusterCheckRuntime(req, native, processSupport) }); err != nil {
		return err
	}
	if err := check("account", func() error { return validateClusterCheckAccount(req) }); err != nil {
		return err
	}
	if err := check("directory", func() error { return validateClusterCheckDirectory(ctx, req) }); err != nil {
		return err
	}
	if !req.NoService {
		return check("service", func() error { return validateClusterCheckService(ctx, req) })
	}
	return nil
}

func validateClusterCheckRequest(req clusterCheckRequest) error {
	if len(req.Token) != 64 || strings.ContainsAny(req.Token, "\r\n") {
		return errors.New("invalid check challenge")
	}
	ip := net.ParseIP(req.Host)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() {
		return errors.New("cluster address must be a usable non-loopback IPv4 address")
	}
	if len(req.Targets) != 3 || req.Targets[req.Host] != req.Token {
		return errors.New("check requires three target-specific challenges including this node")
	}
	for host, token := range req.Targets {
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || len(token) != 64 || strings.ContainsAny(token, "\r\n") {
			return errors.New("invalid target-specific check challenge")
		}
	}
	if err := validateClusterInstallPorts(req.HTTP, req.Enrollment, req.Database); err != nil {
		return err
	}
	return nil
}

func validateClusterCheckRuntime(req clusterCheckRequest, native bool, processSupport func() error) error {
	if runtime.GOOS != "linux" || !native {
		return errors.New("check executable must include Linux native dqlite support")
	}
	if req.Arch != runtime.GOARCH {
		return fmt.Errorf("executable architecture %s differs from expected %s", runtime.GOARCH, req.Arch)
	}
	if err := processSupport(); err != nil {
		return err
	}
	return nil
}

func validateClusterCheckAccount(req clusterCheckRequest) error {
	account, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve actual SSH account: %w", err)
	}
	if account.Username != req.User {
		return fmt.Errorf("actual SSH account %s differs from configured %s", account.Username, req.User)
	}
	return nil
}

func validateClusterCheckDirectory(ctx context.Context, req clusterCheckRequest) error {
	if err := validateClusterInstallDir(req.Dir); err != nil {
		return err
	}
	if _, err := os.Lstat(req.Dir); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("installation directory already exists (including symlink)")
		}
		return fmt.Errorf("inspect installation directory: %w", err)
	}
	parent := filepath.Dir(req.Dir)
	if !req.NoService {
		// Inspect as the SSH account first: it must be able to traverse the
		// existing ancestors after sudo creates the private installation tree.
		ancestor := parent
		for {
			_, err := os.Lstat(ancestor)
			if !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return fmt.Errorf("inspect installation parent: %w", err)
				}
				break
			}
			ancestor = filepath.Dir(ancestor)
		}
		real, err := filepath.EvalSymlinks(ancestor)
		if err != nil || real != ancestor {
			return errors.New("installation parent contains a symlink or is inaccessible")
		}
		command := clusterInstallDirectoryCommand(clusterInstallNode{Dir: req.Dir, SSH: clusterInstallSSH{User: req.User}}, false, false)
		raw, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
		if err != nil {
			return fmt.Errorf("sudo cannot create the installation directory: %s: %w", strings.TrimSpace(string(raw)), err)
		}
		return nil
	}
	real, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("installation parent must already exist: %w", err)
	}
	if real != parent {
		return errors.New("installation parent contains a symlink")
	}
	// Creating and immediately deleting an exclusive scratch file checks actual
	// account write access, including ACLs/read-only mounts, unlike mode bits.
	scratch, err := os.CreateTemp(parent, ".gpuworks-cluster-check-*")
	if err != nil {
		return fmt.Errorf("installation parent is not writable by %s: %w", req.User, err)
	}
	name := scratch.Name()
	closeErr := scratch.Close()
	removeErr := os.Remove(name)
	if err := errors.Join(closeErr, removeErr); err != nil {
		return fmt.Errorf("remove parent write probe: %w", err)
	}
	return nil
}

func validateClusterCheckService(ctx context.Context, req clusterCheckRequest) error {
	if info, err := os.Stat("/run/systemd/system"); err != nil || !info.IsDir() {
		return errors.New("systemd is required; /run/systemd/system is unavailable")
	}
	if raw, err := exec.CommandContext(ctx, "sudo", "-n", "true").CombinedOutput(); err != nil {
		return fmt.Errorf("passwordless sudo -n is required: %s: %w", strings.TrimSpace(string(raw)), err)
	}
	unit := clusterInstallUnit(req.Dir)
	for _, base := range []string{"/etc/systemd/system", "/run/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"} {
		if _, err := os.Lstat(filepath.Join(base, unit)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("service %s already exists or cannot be inspected in %s", unit, base)
		}
	}
	raw, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=LoadState", "--value").CombinedOutput()
	if err != nil || strings.TrimSpace(string(raw)) != "not-found" {
		return fmt.Errorf("service %s is already loaded or cannot be inspected: %s", unit, strings.TrimSpace(string(raw)))
	}
	return nil
}
