package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// PrepareTraversal lets a dedicated runtime identity traverse dir, the
// agent-owned state directory that holds the runtime's own files (patched
// PRoot, runtime mounts) next to private agent state such as identity.json.
// A state directory the agent creates itself is 0700: the logger makes it
// before any runtime is provisioned.
//
// The grant is search (x) only, never listing or writing, on the narrowest
// class that works:
//   - dir is already traversable for owner: unchanged;
//   - dir grants its group nothing, or its group is already the runtime's
//     primary group: the group becomes the runtime's and gains x (0700 -> 0710);
//   - dir's group holds permissions that a group change would revoke or hand
//     to the runtime: others gain x instead (0750 -> 0751).
//
// dir must belong to the agent and not be writable by group or others;
// anything else is reported, not repaired. Except for AutoDL's /root search
// permission, ancestors are left to CheckAncestorTraversal: whether one is fatal depends
// on what the caller is about to do. Callers run this before each use so that a
// state directory reset after provisioning is repaired again; the traversable
// case costs a stat.
func PrepareTraversal(dir string, owner MountOwner) error {
	if err := PrepareAutoDLTraversal(dir, owner); err != nil {
		return err
	}
	// The state path is agent configuration and may itself be a symlink:
	// resolve it, then change only the opened directory.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("resolve runtime state directory %s: %w", dir, err)
	}
	return grantTraversal(resolved, owner)
}

func grantTraversal(dir string, owner MountOwner) error {
	f, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open runtime state directory %s: %w", dir, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat runtime state directory %s: %w", dir, err)
	}
	if HasPermission(info, owner, 0o1) {
		return nil
	}
	uid, gid, ok := FileOwner(info)
	mode := info.Mode().Perm()
	if !ok || uid != os.Geteuid() || mode&0o022 != 0 {
		return traversalError(dir, info, owner, "must belong to the agent and not be writable by group or others to grant runtime traversal")
	}
	if gid != owner.GID && mode&0o070 != 0 {
		slog.Warn("runtime state directory group holds its own permissions; granting traversal to others instead",
			"dir", dir, "mode", fmt.Sprintf("%04o", mode), "gid", gid, "run_gid", owner.GID)
		if err := f.Chmod(info.Mode() | 0o001); err != nil {
			return fmt.Errorf("grant runtime traversal on %s: %w", dir, err)
		}
		return nil
	}
	if gid != owner.GID {
		if err := f.Chown(-1, owner.GID); err != nil {
			return fmt.Errorf("set runtime group on %s: %w", dir, err)
		}
	}
	if err := f.Chmod(info.Mode() | 0o010); err != nil {
		if gid != owner.GID {
			if rollback := f.Chown(-1, gid); rollback != nil {
				return fmt.Errorf("grant runtime traversal on %s: %w; restore group %d: %v", dir, err, gid, rollback)
			}
		}
		return fmt.Errorf("grant runtime traversal on %s: %w", dir, err)
	}
	return nil
}

// CheckAncestorTraversal reports the first directory from name up to / that
// owner cannot traverse. It never changes anything.
func CheckAncestorTraversal(name string, owner MountOwner) error {
	for current := filepath.Clean(name); ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err != nil {
			return fmt.Errorf("stat parent %s: %w", current, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %s is not a directory", current)
		}
		if !HasPermission(info, owner, 0o1) {
			return traversalError(current, info, owner, "is not traversable")
		}
		if next := filepath.Dir(current); next == current {
			return nil
		}
	}
}

// HasPermission reports whether owner holds every bit of need (0o4 read,
// 0o2 write, 0o1 search/execute) on info through its user, primary group or
// other class, the way the kernel picks exactly one class.
func HasPermission(info os.FileInfo, owner MountOwner, need os.FileMode) bool {
	uid, gid, ok := FileOwner(info)
	if !ok {
		return false
	}
	mode := info.Mode().Perm()
	var bits os.FileMode
	switch {
	case owner.UID == uid:
		bits = (mode >> 6) & 0o7
	case owner.GID == gid:
		bits = (mode >> 3) & 0o7
	default:
		bits = mode & 0o7
	}
	return bits&need == need
}

// FileOwner returns the uid and gid recorded in info.
func FileOwner(info os.FileInfo) (int, int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}

func traversalError(name string, info os.FileInfo, owner MountOwner, reason string) error {
	uid, gid, _ := FileOwner(info)
	return fmt.Errorf("%s %s (run_uid=%d run_gid=%d owner_uid=%d owner_gid=%d mode=%04o)", name, reason, owner.UID, owner.GID, uid, gid, info.Mode().Perm())
}
