//go:build linux || darwin

package expose

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// openStateDirectory pins each real directory before opening its child. Never
// chmod through path names: even a link swapped in after validation must not
// change permissions outside the store.
func openStateDirectory(path string, create bool) (*os.File, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// macOS exposes these OS-owned aliases by default (including TMPDIR).
	// Open their fixed real targets, rather than following a user-controlled
	// component during traversal. Other symbolic links remain unsupported.
	if runtime.GOOS == "darwin" {
		for _, prefix := range []string{"/var", "/tmp", "/etc"} {
			if absolute == prefix || strings.HasPrefix(absolute, prefix+"/") {
				if target, err := os.Readlink(prefix); err == nil && (target == "private"+prefix || target == "/private"+prefix) {
					absolute = "/private" + absolute
				}
			}
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.TrimPrefix(absolute, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == unix.ENOENT && create {
			if mkdirErr := unix.Mkdirat(fd, part, 0o775); mkdirErr != nil && mkdirErr != unix.EEXIST {
				unix.Close(fd)
				return nil, mkdirErr
			}
			next, err = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		unix.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("open real state directory %s: %w", absolute, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), absolute), nil
}

func ensureStateDirectory(path string) error {
	dir, err := openStateDirectory(path, true)
	if err != nil {
		return err
	}
	defer dir.Close()
	_ = dir.Chmod(dirMode)
	return nil
}

func openStateLock(path string) (*os.File, error) {
	dir, err := openStateDirectory(filepath.Dir(path), false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), filepath.Base(path), unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0o664)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		file.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		file.Close()
		return nil, fmt.Errorf("state lock %s is not an unshared regular file", path)
	}
	return file, nil
}

// Keep the directory descriptor through the rename: replacing any parent with
// a symlink cannot redirect the temporary file or the final configuration.
func writeStateFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir, err := openStateDirectory(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer dir.Close()
	var file *os.File
	var name string
	for attempt := 0; attempt < 10; attempt++ {
		name = filepath.Base(path) + ".tmp" + NewRevision()
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == unix.EEXIST {
			continue
		}
		if err != nil {
			return err
		}
		file = os.NewFile(uintptr(fd), name)
		break
	}
	if file == nil {
		return fmt.Errorf("cannot create unique state file for %s", path)
	}
	defer func() {
		file.Close()
		if name != "" {
			_ = unix.Unlinkat(int(dir.Fd()), name, 0)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), filepath.Base(path)); err != nil {
		return err
	}
	name = ""
	return nil
}
