package selfinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/launchd"
)

// validateUninstall runs before confirmation and again after taking the lock.
// In particular it never follows a configurable directory symlink while
// recursively removing data. Symlinks inside that tree are unlinked by rm,
// not followed; nested mount points must be detached separately.
func (i *Installer) validateUninstall(paths []string, purge bool) error {
	if err := i.validateUninstallPID(); err != nil {
		return err
	}
	if err := safeUninstallDirectory(i.layout.Dir); err != nil {
		return err
	}
	if resolved, err := resolveUninstallPath(i.layout.LockPath()); err != nil {
		return err
	} else if resolved != i.layout.LockPath() {
		return fmt.Errorf("upgrade lock %s must not be a symbolic link", i.layout.LockPath())
	}
	for _, path := range []string{i.layout.UnitPath(), i.layout.LaunchdPath()} {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("cannot establish service ownership through symbolic link %s", path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.ReadFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot inspect service definition %s: %w", path, err)
		}
	}
	if unit, err := os.ReadFile(i.layout.UnitPath()); err == nil && unitRuns(unit, i.layout.Binary()) {
		if err := i.validateUninstallUnit(unit); err != nil {
			return err
		}
		if err := CheckUninstallServiceOverrides(i.layout.UnitPath()); err != nil {
			return err
		}
	}
	if raw, err := os.ReadFile(i.layout.LaunchdPath()); err == nil {
		spec, err := launchd.ReadSpec(raw)
		if err != nil {
			return err
		}
		if len(spec.Args) > 0 && spec.Args[0] == i.layout.Binary() && !slices.Equal(spec.Args, []string{i.layout.Binary(), "serve", "--config", i.layout.ConfigPath()}) {
			return fmt.Errorf("launch daemon shares this binary with an unrecognized configuration; inspect %s", i.layout.LaunchdPath())
		}
	}
	if purge {
		if err := safeUninstallDirectory(i.layout.DataDir); err != nil {
			return err
		}
		if pathWithin(i.layout.DataDir, i.layout.Dir) {
			return fmt.Errorf("data directory %s contains the installation; refusing recursive cleanup", i.layout.DataDir)
		}
		entries, err := os.ReadDir(i.layout.DataDir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(entries) > 0 && !fileExists(filepath.Join(i.layout.DataDir, "gpuworks.db")) && !fileExists(filepath.Join(i.layout.DataDir, "master.key")) {
			return fmt.Errorf("nonempty data directory %s has no gpuworks database or master key; inspect its ownership before cleanup", i.layout.DataDir)
		}
	}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("cleanup path must be absolute: %s", path)
		}
		for _, root := range []string{"/etc", "/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/usr/local/bin", "/usr/local/sbin", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/proc", "/sys", "/dev"} {
			if pathWithin(root, path) {
				return fmt.Errorf("refusing cleanup of system path %s", path)
			}
		}
		resolved, err := resolveUninstallPath(path)
		if err != nil {
			return err
		}
		if resolved != filepath.Clean(path) {
			return fmt.Errorf("cleanup path %s resolves through a symlink to %s; use an installation with canonical paths", path, resolved)
		}
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		recursive := path == i.layout.DataDir || path == i.layout.TmpDir() || path == i.layout.BackupsDir
		if err == nil && recursive && !info.IsDir() {
			return fmt.Errorf("expected a cleanup directory, found file %s", path)
		}
		if err == nil && info.IsDir() && !recursive {
			return fmt.Errorf("expected a file, found directory %s", path)
		}
		if recursive {
			if err := safeUninstallDirectory(path); err != nil {
				return err
			}
			mounts, err := uninstallMountPoints()
			if err != nil {
				return fmt.Errorf("cannot inspect mounts before cleaning %s: %w", path, err)
			}
			for _, mount := range mounts {
				if pathWithin(path, mount) {
					return fmt.Errorf("cleanup path %s contains mount point %s; detach it before uninstalling", path, mount)
				}
			}
		}
	}
	// The agent shares /opt/gpuworks with the default server installation.
	// Data targeting the agent's state would erase that other installation.
	if purge {
		for _, name := range []string{"gpuworks-agent", "agent-restart.sh", "server.yaml"} {
			if _, err := os.Lstat(filepath.Join(i.layout.DataDir, name)); err == nil {
				return fmt.Errorf("data directory %s contains another installation (%s)", i.layout.DataDir, name)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (i *Installer) validateUninstallUnit(unit []byte) error {
	want := "ExecStart=" + systemdArgs([]string{i.layout.Binary(), "serve", "--config", i.layout.ConfigPath()})
	var starts []string
	legacyData := false
	for _, line := range unitLines(unit) {
		if strings.HasPrefix(line, "ExecStart=") {
			starts = append(starts, line)
		}
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			legacyData = legacyData || firstUnitWord(value) == "GPUWORKS_DATA_DIR="+i.layout.DataDir
		}
	}
	if len(starts) == 1 && starts[0] == want {
		return nil
	}
	// Before server.yaml, install.json and this exact environment assignment
	// associated a unit with its data. Do not migrate merely to uninstall.
	if len(starts) == 1 && legacyData && !fileExists(i.layout.ConfigPath()) && fileExists(i.layout.installJSONPath()) {
		cfg, err := readInstallJSON(i.layout.installJSONPath())
		if err != nil {
			return err
		}
		legacy := "ExecStart=" + systemdArgs([]string{i.layout.Binary(), "serve", "--log-file", cfg.Log.File, "--log-max-size", cfg.Log.MaxSize, "--log-keep", strconv.Itoa(cfg.Log.Keep)})
		if starts[0] == legacy {
			return nil
		}
	}
	return fmt.Errorf("service shares this binary with an unrecognized configuration; inspect %s", i.layout.UnitPath())
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// checkDedicatedDirectory excludes system and home roots before recursive
// filesystem changes. Callers may protect additional application roots for
// destructive operations; those roots can still be valid install locations.
func checkDedicatedDirectory(path string, protectedApplicationRoots ...string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("directory must be absolute: %s", path)
	}
	clean := filepath.Clean(path)
	canonical, err := resolveUninstallPath(clean)
	if err != nil {
		return err
	}
	// These directories can contain an installation, but cannot themselves
	// be the installation or its dedicated data directory.
	protected := []string{"/", "/opt", "/usr", "/usr/local", "/bin", "/sbin", "/etc", "/var", "/var/lib", "/var/log", "/var/tmp", "/tmp", "/home", "/root", "/Users", "/private", "/private/tmp", "/private/var", "/private/var/lib", "/private/var/log", "/private/var/tmp", "/srv", "/mnt", "/media", "/run", "/dev", "/proc", "/sys", "/lib", "/lib64", "/boot"}
	protected = append(protected, protectedApplicationRoots...)
	if home, err := os.UserHomeDir(); err == nil {
		protected = append(protected, filepath.Clean(home))
	}
	for _, root := range protected {
		// Protect the directory identity too: /bin may be /usr/bin,
		// /var may be /private/var, and HOME may itself be a symlink.
		resolved, resolveErr := filepath.EvalSymlinks(root)
		for _, candidate := range []string{clean, canonical} {
			if candidate == root || (resolveErr == nil && candidate == resolved) {
				return fmt.Errorf("refusing recursive changes to broad directory %s", path)
			}
		}
	}
	// User home directories are shared workspace, including those of users
	// other than the operator running the command via sudo.
	for _, root := range []string{"/home", "/Users"} {
		resolved, resolveErr := filepath.EvalSymlinks(root)
		for _, candidate := range []string{clean, canonical} {
			parent := filepath.Dir(candidate)
			if parent == root || (resolveErr == nil && parent == resolved) {
				return fmt.Errorf("refusing recursive changes to user home directory %s", path)
			}
		}
	}
	return nil
}

func safeUninstallDirectory(path string) error {
	if err := checkDedicatedDirectory(path, "/opt/gpuworks"); err != nil {
		return err
	}
	clean := filepath.Clean(path)
	resolved, err := resolveUninstallPath(clean)
	if err != nil {
		return err
	}
	if resolved != clean {
		return fmt.Errorf("directory %s resolves through a symlink to %s; use canonical paths", path, resolved)
	}
	return nil
}

// Resolve the existing prefix even when the final target was already removed.
func resolveUninstallPath(path string) (string, error) {
	probe := filepath.Clean(path)
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for n := len(missing) - 1; n >= 0; n-- {
				resolved = filepath.Join(resolved, missing[n])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("inspect cleanup path %s: %w", path, err)
		}
		// A dangling symlink also returns ENOENT. It is not a missing path.
		if info, statErr := os.Lstat(probe); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("cleanup path contains dangling symlink %s", probe)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}
