package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// HardenSensitiveFiles protects only the established installation's known
// credentials and recognized snapshots, not unrelated adjacent trees.
func HardenSensitiveFiles(layout Layout) error {
	policy, err := SensitiveFilePolicy(layout)
	if err != nil {
		return err
	}
	return hardenFilesWithPolicy(layout, policy)
}
func hardenFilesWithPolicy(layout Layout, policy securefile.Policy) error {
	paths, err := sensitivePaths(layout)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := securefile.Protect(path, policy); err != nil {
			return err
		}
	}
	return nil
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
	paths, err := sensitivePaths(i.layout)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := securefile.Protect(path, policy); err != nil {
			// Protect changes the opened file descriptor. A privileged chmod or
			// chgrp by pathname could follow a replacement symlink before any
			// post-check, so retry the whole command with appropriate ownership.
			return fmt.Errorf("%w; retry install or upgrade as the file owner or root", err)
		}
	}
	return nil
}
