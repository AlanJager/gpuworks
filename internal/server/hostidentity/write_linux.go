//go:build linux

package hostidentity

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"gpuworks.ai/gpuworks/internal/securefile"
)

// Write is `gpuworks host-identity --write path [--group group]`: run as
// root before the service starts (the unit's ExecStartPre), it reads the
// facts directly and replaces path with them, owned by root and group
// with mode 0440, so the service account can read the file but neither
// change nor replace it. The directory of path must be owned by root with
// mode 0755; Write creates it that way when it is missing and refuses one
// with another owner, mode or extended ACL. The file is written to a new
// temporary file
// in that directory (O_CREAT|O_EXCL|O_NOFOLLOW, so a planted symlink is
// never followed), owned and moded through its descriptor, synced, and
// renamed into place: a reader sees the old file or the new one, never a
// part of either.
func Write(path, group string) error {
	if err := checkTarget(path); err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}
	gid, err := groupID(group)
	if err != nil {
		return err
	}
	return writeAt(target{dir: filepath.Dir(path), name: filepath.Base(path), uid: 0, gid: gid, dirUID: 0},
		Read(os.DirFS("/")).Marshal())
}

// target is where writeAt writes and whom it trusts: the file's owner, and
// the owner its directory must have (root, except in tests run without it).
type target struct {
	dir, name string
	uid, gid  int
	dirUID    int
	// tempName names the temporary file; nil picks a random name.
	tempName func() string
	// sync flushes the temporary file; nil is (*os.File).Sync.
	sync func(*os.File) error
}

func writeAt(t target, data []byte) (err error) {
	if err := ensureDir(t.dir); err != nil {
		return err
	}
	dfd, err := syscall.Open(t.dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w (it must be a directory, not a symlink)", t.dir, err)
	}
	dir := os.NewFile(uintptr(dfd), t.dir)
	defer dir.Close()
	// Check the directory through the descriptor every later step uses, so
	// what is checked is what is written into.
	var st syscall.Stat_t
	if err := syscall.Fstat(dfd, &st); err != nil {
		return fmt.Errorf("stat %s: %w", t.dir, err)
	}
	if int(st.Uid) != t.dirUID || st.Mode&0o7777 != dirMode {
		return fmt.Errorf("%s is owned by uid %d with mode %#o; it must be owned by uid %d with mode %#o",
			t.dir, st.Uid, st.Mode&0o7777, t.dirUID, dirMode)
	}
	// A default ACL can survive 0600 creation with a disabled mask and
	// become readable by another user when the file is chmod'ed to 0440.
	if err := securefile.CheckDirectoryACL(dir); err != nil {
		return err
	}
	tmp := randomTempName()
	if t.tempName != nil {
		tmp = t.tempName()
	}
	fd, err := syscall.Openat(dfd, tmp, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(t.dir, tmp), err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(t.dir, tmp))
	renamed := false
	defer func() {
		if !renamed {
			_ = syscall.Unlinkat(dfd, tmp)
		}
	}()
	sync := t.sync
	if sync == nil {
		sync = (*os.File).Sync
	}
	if err := writeAndSeal(f, data, t.uid, t.gid, sync); err != nil {
		return err
	}
	if err := syscall.Renameat(dfd, tmp, dfd, t.name); err != nil {
		return fmt.Errorf("rename into %s: %w", filepath.Join(t.dir, t.name), err)
	}
	renamed = true
	return syscall.Fsync(dfd)
}

// writeAndSeal writes data and gives the file its owner and mode through
// the descriptor, then flushes and closes it.
func writeAndSeal(f *os.File, data []byte, uid, gid int, sync func(*os.File) error) error {
	_, err := f.Write(data)
	if err == nil {
		err = f.Chown(uid, gid)
	}
	if err == nil {
		err = f.Chmod(fileMode)
	}
	if err == nil {
		err = sync(f)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", f.Name(), err)
	}
	return nil
}

// ensureDir creates dir with mode 0755 when it is missing; its parent
// must exist. An existing one is left for writeAt to check.
func ensureDir(dir string) error {
	err := os.Mkdir(dir, dirMode)
	switch {
	case err == nil:
		return os.Chmod(dir, dirMode) // the umask may have narrowed it
	case errors.Is(err, fs.ErrExist):
		return nil
	default:
		return fmt.Errorf("create %s: %w", dir, err)
	}
}

func randomTempName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return ".host-identity-" + hex.EncodeToString(b[:]) + ".tmp"
}
