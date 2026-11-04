package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// writeFileAtomic replaces path in one step: readers either see the old file or
// the new one, never a half-written one. The temporary file is created next to
// the target so the rename stays within one filesystem.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// The tree is handed to the service group at the end of an install, so the
// first install into a fresh /opt happens while the operator cannot write
// there yet. The fallback stages privately before publishing with a rename;
// an operator without sudo gets the original direct-write error.

// canSudo reports whether a failed direct write has anywhere to fall back to.
func (i *Installer) canSudo() bool {
	return i.deps.Euid() == 0 || i.deps.hasBinary("sudo")
}

// writeInstalled stores content at path, falling back to a private sudo stage.
func (i *Installer) writeInstalled(ctx context.Context, path string, content []byte, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeFileAtomic(path, content, mode); err == nil {
		return nil
	} else if !i.canSudo() {
		return err
	}
	return i.writePrivileged(ctx, path, bytes.NewReader(content), int64(len(content)), mode)
}

// copyInstalled copies src to dst, falling back to a sudo-staged rename. A
// direct `sudo cp` would die on ETXTBSY whenever the running service keeps
// the target mapped; staging a temporary file and renaming it over the target
// is exactly what copyFileAtomic does when it can write.
func (i *Installer) copyInstalled(ctx context.Context, src, dst string, mode os.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := copyFileAtomic(src, dst, mode); err == nil {
		return nil
	} else if !i.canSudo() {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	return i.writePrivileged(ctx, dst, in, info.Size(), mode)
}

const privilegedStagePrefix = ".gpuworks-write."

// Positional parameters are paths, mode and expected size; all content stays
// on stdin. The empty file is private before cat starts, even with umask022.
// A short/failed input must never replace the old destination.
const privilegedWriteScript = `set -eu
umask 077
cd "$1"
trap 'rm -f -- content' EXIT
trap 'exit 1' HUP INT TERM
if [ -d "$2" ] && [ ! -L "$2" ]; then exit 1; fi
set -C
: > content
set +C
chmod 0600 content
cat > content
actual=$(wc -c < content)
[ "$actual" -eq "$4" ]
chmod "$3" content
mv "$5" -- content "$2"
`

// writePrivileged is intentionally limited to a tree writable only by root,
// this operator, or the already authorized platform group. A private stage in
// an untrusted writable parent could itself be renamed/replaced by a symlink.
func (i *Installer) writePrivileged(ctx context.Context, path string, content io.Reader, size int64, mode os.FileMode) (result error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("privileged write path contains a control character")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return fmt.Errorf("inspect privileged write parent; run the whole install or upgrade with sudo: %w", err)
	}
	policy, err := i.sensitiveFilePolicy()
	if err != nil {
		return err
	}
	group := -1
	if gid, trusted := policy.GroupID(); trusted {
		group = gid
	}
	parentInfo, err := trustedWriteParent(parent, group)
	if err != nil {
		return err
	}
	target := filepath.Join(parent, filepath.Base(absolute))
	stage, err := i.deps.Run.Output(ctx, Command{Path: "mktemp", Args: []string{"-d", filepath.Join(parent, privilegedStagePrefix+"XXXXXXXXXX")}, Sudo: true})
	if err != nil {
		return err
	}
	if !filepath.IsAbs(stage) || filepath.Clean(stage) != stage || filepath.Dir(stage) != parent || strings.ContainsAny(stage, "\r\n") || !strings.HasPrefix(filepath.Base(stage), privilegedStagePrefix) {
		return errors.New("privileged writer returned an invalid private staging path")
	}
	suffix := strings.TrimPrefix(filepath.Base(stage), privilegedStagePrefix)
	if len(suffix) != 10 || strings.ContainsFunc(suffix, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z')
	}) {
		return errors.New("privileged writer returned an invalid random staging name")
	}
	stageInfo, err := os.Lstat(stage)
	if err != nil {
		return err
	}
	if !stageInfo.IsDir() || stageInfo.Mode().Perm() != 0o700 || !trustedWriteOwner(stageInfo) {
		return errors.New("privileged staging directory is not private or has an unexpected owner")
	}
	defer func() {
		currentParent, err := trustedWriteParent(parent, group)
		if err != nil || !os.SameFile(parentInfo, currentParent) {
			result = errors.Join(result, errors.New("privileged write parent changed; refusing staging cleanup"), err)
			return
		}
		current, err := os.Lstat(stage)
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil || !os.SameFile(stageInfo, current) || !current.IsDir() || current.Mode().Perm() != 0o700 {
			result = errors.Join(result, errors.New("privileged staging identity changed; refusing cleanup"), err)
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := i.deps.Run.Run(cleanup, Command{Path: "rm", Args: []string{"-rf", "--", stage}, Sudo: true}); err != nil {
			result = errors.Join(result, fmt.Errorf("clean private staging directory: %w", err))
		}
	}()
	currentParent, err := trustedWriteParent(parent, group)
	if err != nil || !os.SameFile(parentInfo, currentParent) {
		return errors.Join(errors.New("privileged write parent changed before writing"), err)
	}
	rename := "-fT"
	if runtime.GOOS == "darwin" {
		rename = "-fh"
	}
	return i.deps.Run.Run(ctx, Command{Path: "sh", Args: []string{"-c", privilegedWriteScript, "gpuworks-atomic-write", stage, target, fmt.Sprintf("%04o", mode.Perm()), strconv.FormatInt(size, 10), rename}, Sudo: true, Stdin: content})
}

func trustedWriteOwner(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (stat.Uid == 0 || int(stat.Uid) == os.Getuid())
}

func trustedWriteParent(parent string, group int) (os.FileInfo, error) {
	var first os.FileInfo
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return nil, fmt.Errorf("inspect privileged write parent; run the whole install or upgrade with sudo: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || !trustedWriteOwner(info) ||
			(info.Mode().Perm()&0o020 != 0 && stat.Gid != 0 && int(stat.Gid) != group) ||
			(info.Mode().Perm()&0o002 != 0 && (current == parent || info.Mode()&os.ModeSticky == 0)) {
			return nil, fmt.Errorf("untrusted writer can replace privileged path %s; run the whole install or upgrade with sudo", current)
		}
		dir, err := os.Open(current)
		if err != nil {
			return nil, fmt.Errorf("open privileged write parent; run the whole install or upgrade with sudo: %w", err)
		}
		opened, statErr := dir.Stat()
		aclErr := securefile.CheckDirectoryACL(dir)
		closeErr := dir.Close()
		if statErr != nil || !os.SameFile(info, opened) {
			return nil, errors.Join(fmt.Errorf("privileged write parent changed: %s", current), statErr, closeErr)
		}
		if aclErr != nil || closeErr != nil {
			return nil, errors.Join(fmt.Errorf("inspect privileged write directory ACL at %s; repair unexpected ACLs before retrying", current), aclErr, closeErr)
		}
		if first == nil {
			first = info
		}
		if current == filepath.Dir(current) {
			return first, nil
		}
	}
}

// copyFileAtomic copies src to dst through a temporary file, so a failure never
// leaves a partial program where the service expects one. The copy preserves
// only mode, which is what an installed binary needs.
func copyFileAtomic(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// fileExists reports whether path exists as a regular file or directory.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// mkdirAll creates dir with the given mode, reported with the path for context.
func mkdirAll(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	return nil
}
