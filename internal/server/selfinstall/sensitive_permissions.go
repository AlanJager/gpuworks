package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// HardenSensitiveFiles protects only the established installation's known
// credentials and recognized snapshots, not unrelated adjacent trees. A
// snapshot this process may not open or chmod (one an upgrade wrote as root or
// as another operator) is rollback material only, so it is reported to warn
// instead of failing; credentials always fail.
func HardenSensitiveFiles(layout Layout, warn func(path string, err error)) error {
	policy, err := SensitiveFilePolicy(layout)
	if err != nil {
		return err
	}
	paths, err := sensitivePaths(layout)
	if err != nil {
		return err
	}
	for _, path := range paths {
		err := protectFile(path, policy)
		if err != nil && warn != nil && errors.Is(err, os.ErrPermission) && isBackupPath(layout, path) {
			warn(path, err)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func hardenFilesWithPolicy(layout Layout, policy securefile.Policy, activeDatabases ...string) error {
	paths, err := sensitivePaths(layout)
	if err != nil {
		return err
	}
	// In-service backup reads a separate SQLite snapshot. Live database files
	// must be checked without opening/closing descriptors that cancel locks.
	for _, path := range paths {
		if slices.Contains(activeDatabases, path) {
			if err := verifyActiveDatabaseFile(path, policy); err != nil {
				return err
			}
		}
	}
	for _, path := range paths {
		if slices.Contains(activeDatabases, path) {
			continue
		}
		if err := protectFile(path, policy); err != nil {
			return err
		}
	}
	return nil
}

func verifyActiveDatabaseFile(path string, policy securefile.Policy) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := policy.Check(before); err != nil {
		return fmt.Errorf("verify active database %s without repairing permissions: %w", path, err)
	}
	if _, group := policy.GroupID(); group {
		if err := securefile.CheckFileACL(path); err != nil {
			return err
		}
	}
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return fmt.Errorf("active database %s changed during permission inspection", path)
	}
	return policy.Check(after)
}

// protectHandedOverFile is for files a privileged chown just handed to the
// service account: this shell may not have picked up the new group yet, so a
// file it may not open or chmod is accepted when a no-follow check shows it
// already satisfies the policy. Paths that go on to read the files keep the
// strict check so they refuse before changing anything.
func protectHandedOverFile(path string, policy securefile.Policy) error {
	err := protectFile(path, policy)
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return err
	}
	if verifyErr := securefile.Verify(path, policy); verifyErr != nil {
		return fmt.Errorf("%w (%v)", err, verifyErr)
	}
	return nil
}

// protectFile is replaced in tests: an unprivileged test cannot create a file
// it may not open whose mode still matches the policy.
var protectFile = func(path string, policy securefile.Policy) error {
	return securefile.Protect(path, policy)
}

func isBackupPath(layout Layout, path string) bool {
	return filepath.Dir(path) == filepath.Clean(layout.BackupsDir)
}

func sensitivePaths(layout Layout) ([]string, error) {
	if layout.DataDir == "" || layout.BackupsDir == "" {
		return nil, errors.New("sensitive file protection requires an installation layout")
	}
	var paths []string
	add := func(path string) error {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("protect %s: expected a regular file, not a symlink or special file", path)
		}
		paths = append(paths, path)
		return nil
	}
	for _, name := range []string{"master.key", "initial-credentials", "gpuworks.db", "gpuworks.db-wal", "gpuworks.db-shm", "gpuworks.db-journal"} {
		if err := add(filepath.Join(layout.DataDir, name)); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(layout.BackupsDir)
	if errors.Is(err, os.ErrNotExist) {
		return paths, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("protect backups %s: expected a directory, not a symlink or special file", layout.BackupsDir)
	}
	entries, err := os.ReadDir(layout.BackupsDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if _, ok := backupStampAt(entry.Name()); !ok {
			continue
		}
		if err := add(filepath.Join(layout.BackupsDir, entry.Name())); err != nil {
			return nil, err
		}
	}
	return paths, nil
}

// SensitiveFilePolicy resolves an authorized group identity from a unit bound
// to this installation. Unknown, standalone and Darwin contexts stay private.
func SensitiveFilePolicy(layout Layout) (securefile.Policy, error) {
	return sensitiveFilePolicy(layout, runtime.GOOS, user.LookupGroup)
}

func sensitiveFilePolicy(layout Layout, goos string, lookup func(string) (*user.Group, error)) (securefile.Policy, error) {
	if goos != "linux" {
		return securefile.Policy{}, nil
	}
	unit, err := os.ReadFile(layout.UnitPath())
	if errors.Is(err, os.ErrNotExist) {
		return securefile.Policy{}, nil
	}
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("identify credential service policy: %w", err)
	}
	if !unitRuns(unit, layout.Binary()) {
		return securefile.Policy{}, nil
	}
	account, group := unitUser(unit), unitGroup(unit)
	if account == "" || account == "root" {
		return securefile.Policy{}, nil
	}
	// Default installations already delegated platform administration to the
	// reserved group before units recorded Group=. Do not extend that exception
	// to arbitrary custom accounts and their primary groups.
	legacy := group == "" && account == DefaultServiceUser
	if legacy {
		group = DefaultCredentialGroup
	}
	if group != DefaultCredentialGroup {
		return securefile.Policy{}, nil
	}
	if lookup == nil {
		return securefile.Policy{}, errors.New("no credential group lookup configured")
	}
	record, err := lookup(group)
	if err != nil {
		var unknown user.UnknownGroupError
		if legacy && errors.As(err, &unknown) {
			return securefile.Policy{}, nil
		}
		return securefile.Policy{}, fmt.Errorf("resolve authorized credential group %s: %w", group, err)
	}
	gid, err := strconv.Atoi(record.Gid)
	if err != nil {
		return securefile.Policy{}, fmt.Errorf("invalid credential group gid: %w", err)
	}
	return securefile.TrustedGroup(gid)
}

func unitGroup(unit []byte) string {
	for _, line := range unitLines(unit) {
		if value, ok := strings.CutPrefix(line, "Group="); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (i *Installer) sensitiveFilePolicy() (securefile.Policy, error) {
	return sensitiveFilePolicy(i.layout, runtime.GOOS, i.deps.LookupGroup)
}

// Check existing sensitive files before replacing installation files or stopping
// its service. A fresh installation has no credentials or group binding yet.
func (i *Installer) preflightSensitiveFiles(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	paths, err := sensitivePaths(i.layout)
	if err != nil || len(paths) == 0 {
		return err
	}
	policy, err := i.sensitiveFilePolicy()
	if err != nil {
		return err
	}
	return i.hardenSensitiveFiles(ctx, policy)
}

func (i *Installer) hardenSensitiveFiles(ctx context.Context, policy securefile.Policy) error {
	return i.hardenWith(ctx, policy, protectFile)
}

// hardenHandedOverFiles finishes chownTree's privileged handoff.
func (i *Installer) hardenHandedOverFiles(ctx context.Context, policy securefile.Policy) error {
	return i.hardenWith(ctx, policy, protectHandedOverFile)
}

func (i *Installer) hardenWith(ctx context.Context, policy securefile.Policy, protect func(string, securefile.Policy) error) error {
	paths, err := sensitivePaths(i.layout)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := protect(path, policy); err != nil {
			// Protect changes the opened file descriptor. A privileged chmod or
			// chgrp by pathname could follow a replacement symlink before any
			// post-check, so retry the whole command with appropriate ownership.
			return fmt.Errorf("%w; retry install or upgrade as the file owner or root", err)
		}
	}
	return nil
}
