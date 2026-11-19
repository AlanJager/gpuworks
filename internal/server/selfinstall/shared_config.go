package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
)

const sharedConfigDirectory = "server-config"
const sharedConfigAlias = sharedConfigDirectory + "/" + serverconfig.FileName

func (l Layout) sharedConfigDir() string { return filepath.Join(l.Dir, sharedConfigDirectory) }
func (l Layout) sharedConfigFile() string {
	return filepath.Join(l.sharedConfigDir(), serverconfig.FileName)
}
func (l Layout) inSharedRoot() bool { return sameDirectory(l.Dir, sharedApplicationRoot) }

// DirOfConfig identifies the installation owning a configuration. A private
// shared-root configuration belongs to the adjacent binary's directory, only
// when the installer's controlled alias verifies it. Other layouts keep the
// established configuration-directory convention.
func DirOfConfig(path string) string {
	if absolute, err := filepath.Abs(path); err == nil {
		layout := NewLayout(filepath.Dir(filepath.Dir(absolute)), "", "")
		if absolute == layout.sharedConfigFile() {
			if migrated, err := layout.sharedConfig(); err == nil && migrated {
				return layout.Dir
			}
		}
	}
	return filepath.Dir(path)
}

// sharedConfig recognizes only the installer's exact compatibility alias.
// Invalid layouts return an error before any install/upgrade/uninstall write;
// callers must never resolve an arbitrary config symlink into a write target.
func (l Layout) sharedConfig() (bool, error) {
	return l.inspectSharedConfig(false)
}

// Only rollback may fill a missing regular leaf beneath the already verified
// canonical alias and real private parent. Normal readers remain strict.
func (l Layout) inspectSharedConfig(restoreMissingLeaf bool) (bool, error) {
	if !l.inSharedRoot() {
		return false, nil
	}
	root, err := resolveUninstallPath(l.Dir)
	if err != nil {
		return false, err
	}
	if root != filepath.Clean(l.Dir) {
		return false, fmt.Errorf("shared installation %s resolves through a symlink", l.Dir)
	}
	rootInfo, err := os.Lstat(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !rootInfo.IsDir() || !trustedWriteOwner(rootInfo) || rootInfo.Mode().Perm()&0o022 != 0 {
		// The older installer's shape gets an actionable error that a
		// privileged install or upgrade migrates (migrateLegacySharedRoot).
		if legacy := legacySharedRoot(l.Dir, rootInfo); legacy != nil {
			return false, legacy
		}
		return false, fmt.Errorf("shared application root %s must remain a trusted non-writable directory (mode %04o)", l.Dir, rootInfo.Mode().Perm())
	}
	rootFile, err := os.Open(l.Dir)
	if err != nil {
		return false, err
	}
	aclErr := securefile.CheckDirectoryACL(rootFile)
	closeErr := rootFile.Close()
	if err := errors.Join(aclErr, closeErr); err != nil {
		return false, err
	}
	for _, path := range []string{l.sharedConfigDir(), l.sharedConfigFile()} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return false, err
		}
		wantDirectory := path == l.sharedConfigDir()
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDirectory || (!wantDirectory && !info.Mode().IsRegular()) {
			return false, fmt.Errorf("shared configuration path %s must not be a symlink or unexpected file type", path)
		}
		if info.Mode().Perm()&0o002 != 0 {
			return false, fmt.Errorf("shared configuration path %s is writable by untrusted users", path)
		}
		if wantDirectory {
			f, err := os.Open(path)
			if err != nil {
				return false, err
			}
			aclErr := securefile.CheckDirectoryACL(f)
			closeErr := f.Close()
			if err := errors.Join(aclErr, closeErr); err != nil {
				return false, err
			}
		} else if err := securefile.CheckFileACL(path); err != nil {
			return false, err
		}
	}
	alias := serverconfig.Path(l.Dir)
	info, err := os.Lstat(alias)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := os.Lstat(l.sharedConfigFile()); !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("private configuration exists without its controlled alias: %s", l.sharedConfigFile())
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("configuration %s is not a regular file", alias)
		}
		if _, err := os.Lstat(l.sharedConfigFile()); !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("private configuration conflicts with legacy %s", alias)
		}
		return false, nil
	}
	target, err := os.Readlink(alias)
	if err != nil {
		return false, err
	}
	if target != sharedConfigAlias || !trustedWriteOwner(info) {
		return false, fmt.Errorf("configuration %s is not the installer's canonical alias", alias)
	}
	if _, err := os.Lstat(l.sharedConfigFile()); err != nil {
		if restoreMissingLeaf && errors.Is(err, os.ErrNotExist) {
			parent, parentErr := os.Lstat(l.sharedConfigDir())
			if parentErr == nil && parent.IsDir() && parent.Mode()&os.ModeSymlink == 0 {
				return true, nil
			}
		}
		return false, fmt.Errorf("controlled configuration alias is dangling: %w", err)
	}
	return true, nil
}

// isolateSharedConfig runs only on mutating installer paths. A legacy file is
// copied byte-for-byte before publishing the compatibility alias atomically.
// A new install supplies its initial rendered configuration instead.
func (i *Installer) isolateSharedConfig(ctx context.Context, initial []byte) (bool, error) {
	migrated, err := i.layout.sharedConfig()
	if err != nil || migrated || !i.layout.inSharedRoot() {
		return false, err
	}
	old := serverconfig.Path(i.layout.Dir)
	original, originalErr := os.Lstat(old)
	if originalErr != nil && !errors.Is(originalErr, os.ErrNotExist) {
		return false, originalErr
	}
	mode := os.FileMode(0o664)
	content, err := os.ReadFile(old)
	if errors.Is(err, os.ErrNotExist) {
		content = initial
	} else if err != nil {
		return false, err
	} else {
		info, err := os.Stat(old)
		if err != nil {
			return false, err
		}
		mode = info.Mode().Perm()
	}
	if content == nil {
		return false, fmt.Errorf("cannot migrate missing configuration %s", old)
	}
	target := i.layout.sharedConfigFile()
	if err := i.writeInstalled(ctx, target, content, mode); err != nil {
		return false, err
	}
	written, err := os.Lstat(target)
	if err != nil {
		return false, err
	}
	// ConfigPath intentionally remains the legacy path until this publication.
	if err := i.publishSharedConfigAlias(ctx); err != nil {
		// A publication can succeed before its private-stage cleanup fails.
		// Once the alias names this file, removing it would lose the config.
		if link, linkErr := os.Readlink(old); linkErr == nil && link == sharedConfigAlias {
			return true, err
		}
		current, currentErr := os.Lstat(old)
		unchanged := originalErr == nil && currentErr == nil && os.SameFile(original, current) && current.Mode().IsRegular()
		if !unchanged && !(errors.Is(originalErr, os.ErrNotExist) && errors.Is(currentErr, os.ErrNotExist)) {
			return false, errors.Join(err, errors.New("configuration changed during alias publication; retained private configuration"))
		}
		currentTarget, targetErr := os.Lstat(target)
		if targetErr != nil || !currentTarget.Mode().IsRegular() || !os.SameFile(written, currentTarget) {
			return false, errors.Join(err, errors.New("private configuration changed; refusing migration cleanup"), targetErr)
		}
		currentContent, readErr := os.ReadFile(target)
		if readErr != nil || !bytes.Equal(currentContent, content) {
			return false, errors.Join(err, errors.New("private configuration contents changed; refusing migration cleanup"), readErr)
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), i.deps.stopTimeout())
		defer cancel()
		removeErr := i.removeUninstallPath(cleanup, target)
		return false, errors.Join(err, removeErr)
	}
	return true, nil
}

const publishConfigAliasScript = `set -eu
cd "$1"
ln -s -- "$3" content
mv "$4" -- content "$2"
`

func (i *Installer) publishSharedConfigAlias(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	alias := serverconfig.Path(i.layout.Dir)
	stage, err := os.MkdirTemp(i.layout.Dir, privilegedStagePrefix)
	if err == nil {
		defer os.RemoveAll(stage)
		link := filepath.Join(stage, "content")
		if err := os.Symlink(sharedConfigAlias, link); err != nil {
			return err
		}
		return os.Rename(link, alias)
	}
	if !i.canSudo() {
		return err
	}
	return i.withPrivilegedStage(ctx, alias, func(stage, target, rename string) error {
		return i.deps.Run.Run(ctx, Command{Path: "sh", Args: []string{"-c", publishConfigAliasScript, "gpuworks-config-alias", stage, target, sharedConfigAlias, rename}, Sudo: true})
	})
}
