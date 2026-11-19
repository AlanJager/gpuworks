//go:build linux

package pluginexec

import (
	"errors"
	"fmt"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/unix"
)

// LandlockABI returns the Landlock ABI version this kernel offers, or 0
// when it has none (older than 5.13, or Landlock not enabled at boot).
func LandlockABI() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0
	}
	return int(v)
}

const (
	accessRead     = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR
	accessReadExec = accessRead | unix.LANDLOCK_ACCESS_FS_EXECUTE
	// accessFile are the rights that apply to a file rather than a
	// directory; a rule on a file may grant only these.
	accessFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	accessDevice = unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	// accessDeviceDir is accessDevice beneath a directory, which may also
	// be listed to find the nodes.
	accessDeviceDir = accessDevice | unix.LANDLOCK_ACCESS_FS_READ_DIR
)

// handledAccess is every filesystem right the kernel's ABI knows: all of
// them are denied unless a rule grants them.
func handledAccess(abi int) uint64 {
	a := uint64(unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK | unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM)
	if abi >= 2 {
		a |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		a |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		a |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}
	return a
}

// applyLandlock restricts the calling thread (and what it executes) to l.
// no_new_privs must already be set. From ABI 6 the plugin also cannot
// signal processes outside its own domain: the server and other plugins.
func applyLandlock(l *Landlock, abi int) error {
	handled := handledAccess(abi)
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	if abi >= 6 {
		attr.Scoped = unix.LANDLOCK_SCOPE_SIGNAL
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock: create ruleset: %w", errno)
	}
	ruleset := int(fd)
	defer unix.Close(ruleset)
	for _, group := range []struct {
		paths  []string
		access uint64
	}{
		{l.ReadExec, accessReadExec},
		{l.ReadWrite, handled},
		{l.Read, accessRead},
		{l.Devices, accessDevice},
		{l.DeviceDirs, accessDeviceDir},
	} {
		for _, path := range group.paths {
			if err := addPathRule(ruleset, path, group.access&handled); err != nil {
				return err
			}
		}
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
		return fmt.Errorf("landlock: restrict self: %w", errno)
	}
	return nil
}

// addPathRule grants access beneath path; a missing path is skipped, and a
// rule on a file keeps only the rights that apply to files.
func addPathRule(ruleset int, path string, access uint64) error {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("landlock: open %s: %w", path, err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("landlock: stat %s: %w", path, err)
	}
	if fs.FileMode(st.Mode)&unix.S_IFMT != unix.S_IFDIR {
		access &= accessFile
	}
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock: rule for %s: %w", path, errno)
	}
	return nil
}
