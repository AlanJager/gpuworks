package selfinstall

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

// Remove the validated config pair last. If unlinking its alias fails, put the
// private file back atomically so the same installation remains discoverable
// for a retry. Never recursively remove the directory's other contents.
func (i *Installer) purgeSharedConfig(ctx context.Context) error {
	if migrated, err := i.layout.sharedConfig(); err != nil {
		return err
	} else if !migrated {
		return errors.New("shared configuration changed before purge")
	}
	path := i.layout.sharedConfigFile()
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	parent, err := os.Lstat(i.layout.sharedConfigDir())
	if err != nil {
		return err
	}
	if runtime.GOOS != "linux" {
		// BSD ln/link utilities can reinterpret a raced target as a
		// directory. Only native os.Link recovery is supported here;
		// establish its directory-write capability before deleting config.
		probe, err := os.CreateTemp(i.layout.sharedConfigDir(), ".config-purge-check-*")
		if err != nil {
			return fmt.Errorf("private configuration purge needs native recovery permission; run the whole uninstall with sudo: %w", err)
		}
		closeErr := probe.Close()
		linkPath := probe.Name() + ".link"
		linkErr := os.Link(probe.Name(), linkPath)
		var removeLinkErr error
		if linkErr == nil {
			removeLinkErr = os.Remove(linkPath)
		}
		removeErr := os.Remove(probe.Name())
		if err := errors.Join(closeErr, linkErr, removeLinkErr, removeErr); err != nil {
			return fmt.Errorf("native private configuration recovery unavailable before purge (run the whole uninstall with sudo if permission denied): %w", err)
		}
	}
	if err := i.removeUninstallPath(ctx, path); err != nil {
		return err
	}
	alias := serverconfig.Path(i.layout.Dir)
	if err := i.removeUninstallPath(ctx, alias); err != nil {
		if _, statErr := os.Lstat(alias); !errors.Is(statErr, os.ErrNotExist) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), i.deps.stopTimeout())
			defer cancel()
			if restoreErr := i.restoreSharedConfigForRetry(cleanup, raw, info.Mode().Perm(), parent); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore private configuration for uninstall retry: %w", restoreErr))
			}
		}
		return err
	}
	_, err = i.removeEmptySharedConfigDir(ctx)
	return err
}

func (i *Installer) restoreSharedConfigForRetry(ctx context.Context, raw []byte, mode os.FileMode, originalParent os.FileInfo) error {
	// Check the missing leaf separately: the normal resolver correctly rejects
	// dangling aliases outside this tightly scoped compensation path.
	alias := serverconfig.Path(i.layout.Dir)
	target, err := os.Readlink(alias)
	if err != nil || target != sharedConfigAlias {
		return errors.Join(errors.New("configuration alias changed during purge"), err)
	}
	aliasInfo, err := os.Lstat(alias)
	if err != nil || !trustedWriteOwner(aliasInfo) {
		return errors.Join(errors.New("configuration alias ownership changed during purge"), err)
	}
	parentInfo, err := os.Lstat(i.layout.sharedConfigDir())
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(parentInfo, originalParent) || parentInfo.Mode().Perm()&0o002 != 0 {
		return errors.Join(errors.New("private configuration parent changed during purge"), err)
	}
	if _, err := os.Lstat(i.layout.sharedConfigFile()); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(errors.New("private configuration appeared during purge; refusing overwrite"), err)
	}
	// The private directory belongs to the stopped service, so its owner is
	// intentionally different from a root CLI. Keep its validated identity;
	// the protected application parent remains trusted and cannot be aliased.
	if _, err := trustedWriteParent(i.layout.Dir, -1); err != nil {
		return err
	}
	f, err := os.Open(i.layout.sharedConfigDir())
	if err != nil {
		return err
	}
	aclErr := securefile.CheckDirectoryACL(f)
	closeErr := f.Close()
	if err := errors.Join(aclErr, closeErr); err != nil {
		return err
	}
	return i.restoreSharedConfigAbsent(ctx, raw, mode)
}

func (i *Installer) removeEmptySharedConfigDir(ctx context.Context) (bool, error) {
	path := i.layout.sharedConfigDir()
	err := os.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return false, nil
	}
	if !i.canSudo() {
		return false, err
	}
	// rmdir only succeeds for an empty directory, including if a new file
	// arrived after the direct attempt. An unrelated file is always retained.
	if err := i.deps.Run.Run(ctx, Command{Path: "rmdir", Args: []string{"--", path}, Sudo: true}); err != nil {
		return false, err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("could not confirm removal of empty configuration directory %s", path)
	}
	return true, nil
}
