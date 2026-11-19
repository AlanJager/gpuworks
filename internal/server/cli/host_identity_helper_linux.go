//go:build linux

package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"gpuworks.ai/gpuworks/internal/securefile"
	"gpuworks.ai/gpuworks/internal/server/hostidentity"
	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

const hostIdentityExecutable = "host-identity-exec"

func manageHostIdentityHelper(install, remove, group string) error {
	// This branch must refuse before opening or creating any destination.
	if os.Geteuid() != 0 {
		return hostidentity.ErrNotRoot
	}
	unitDir := install
	if unitDir == "" {
		unitDir = remove
	}
	if !filepath.IsAbs(unitDir) || filepath.Clean(unitDir) != unitDir || unitDir == "/" {
		return errors.New("host identity helper needs an absolute, clean systemd unit directory")
	}
	root, err := os.Open("/")
	if err != nil {
		return err
	}
	defer root.Close()
	var rootStat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &rootStat); err != nil {
		return err
	}
	if rootStat.Uid != 0 {
		return errors.New("host identity helper filesystem root must be owned by root")
	}
	return hostIdentityHelperAt(root, unitDir, group, remove != "", func() (*os.File, error) {
		return os.Open("/proc/self/exe")
	})
}

// hostIdentityHelperAt uses an opened filesystem root. Tests give it a
// private filesystem root owned by their account; production always opens
// "/" after the root-only guard above. Every descendant must have the same
// owner as that root, no other writers, no symlink, and no extended ACL.
func hostIdentityHelperAt(root *os.File, unitDir, group string, remove bool, source func() (*os.File, error)) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &st); err != nil {
		return err
	}
	if err := trustedIdentityDirectory(root, st.Uid); err != nil {
		return err
	}
	dir, err := openIdentityDirectory(root, unitDir, st.Uid, remove)
	if errors.Is(err, os.ErrNotExist) && remove {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	fd := int(dir.Fd())
	conf := hostIdentityDropIn + ".conf"
	if remove {
		// Fixed basenames, relative to the validated descriptor. Never recurse
		// or remove other drop-ins (or even the directory containing them).
		for _, name := range []string{conf, hostIdentityExecutable} {
			if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
		return dir.Sync()
	}
	helper := filepath.Join(unitDir, selfinstall.ServiceName+".service.d", hostIdentityExecutable)
	content, err := hostIdentityDropInContent(helper, group)
	if err != nil {
		return err
	}
	for _, name := range []string{hostIdentityExecutable, conf} {
		if err := trustedIdentityFile(fd, name, st.Uid); err != nil {
			return err
		}
	}
	in, err := source()
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("host identity helper source is not a readable executable file"), err)
	}
	// Prepare both files before publishing either. A failed source copy or
	// configuration write leaves the old executable and configuration intact.
	exeTmp, err := stageIdentityFile(dir, in, 0o555, int(st.Uid), info.Size())
	if err != nil {
		return err
	}
	// A successful rename consumes the temporary name. Cleanup is best
	// effort on an earlier failure and must preserve the publication error.
	defer func() { _ = unix.Unlinkat(fd, exeTmp, 0) }()
	confTmp, err := stageIdentityFile(dir, strings.NewReader(content), 0o644, int(st.Uid), int64(len(content)))
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(fd, confTmp, 0) }()
	if err := unix.Renameat(fd, exeTmp, fd, hostIdentityExecutable); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return err
	}
	if err := unix.Renameat(fd, confTmp, fd, conf); err != nil {
		return err
	}
	return dir.Sync()
}

func trustedIdentityDirectory(f *os.File, owner uint32) error {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != owner || st.Mode&0o022 != 0 {
		return fmt.Errorf("host identity helper: %s must be a directory owned by uid %d with no group or other writers", f.Name(), owner)
	}
	return securefile.CheckDirectoryACL(f)
}

func openIdentityDirectory(root *os.File, unitDir string, owner uint32, remove bool) (*os.File, error) {
	if !filepath.IsAbs(unitDir) || filepath.Clean(unitDir) != unitDir || unitDir == "/" {
		return nil, errors.New("host identity helper unit directory must be absolute and clean")
	}
	parts := append(strings.Split(strings.TrimPrefix(unitDir, "/"), "/"), selfinstall.ServiceName+".service.d")
	current := root
	for n, part := range parts {
		fd, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && n == len(parts)-1 && !remove {
			// Only the drop-in directory may be created. All ancestors, including
			// the unit directory, must already exist and pass the root-only policy.
			if err = unix.Mkdirat(int(current.Fd()), part, 0o755); err == nil || errors.Is(err, unix.EEXIST) {
				fd, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			}
		}
		if current != root {
			_ = current.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("open host identity helper directory %s: %w", part, err)
		}
		current = os.NewFile(uintptr(fd), part)
		if err := trustedIdentityDirectory(current, owner); err != nil {
			_ = current.Close()
			return nil, err
		}
	}
	return current, nil
}

func trustedIdentityFile(dir int, name string, owner uint32) error {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != owner || st.Mode&0o022 != 0 {
		return fmt.Errorf("host identity helper: %s is not a protected regular file", name)
	}
	// Files also must not carry a masked ACL that chmod could reactivate.
	for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		if _, err := unix.Fgetxattr(fd, attribute, nil); err == nil {
			return fmt.Errorf("host identity helper: %s has an extended ACL", name)
		} else if !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.EOPNOTSUPP) {
			return err
		}
	}
	return nil
}

func stageIdentityFile(dir *os.File, src io.Reader, mode os.FileMode, owner int, size int64) (name string, err error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name = ".host-identity-" + hex.EncodeToString(random[:]) + ".tmp"
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", err
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = unix.Unlinkat(int(dir.Fd()), name, 0)
		}
	}()
	n, err := io.Copy(f, src)
	if err != nil {
		return name, err
	}
	if n != size {
		return name, io.ErrUnexpectedEOF
	}
	if err = f.Chown(owner, -1); err != nil {
		return name, err
	}
	if err = f.Chmod(mode); err != nil {
		return name, err
	}
	if err = f.Sync(); err != nil {
		return name, err
	}
	err = f.Close()
	return name, err
}
