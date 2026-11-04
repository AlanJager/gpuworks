// Package securefile protects persistent secrets using an owner-only policy
// or an explicitly authorized group identity, never an incidental primary gid.
package securefile

import (
	"errors"
	"fmt"
	"io"
	"os"
)

const OwnerMode os.FileMode = 0600
const GroupMode os.FileMode = 0660

// Policy is derived from trusted runtime installation facts, not persisted.
// Its zero value permits only the owner. A group policy binds the actual file
// gid before granting any group access.
type Policy struct {
	group bool
	gid   int
}

func TrustedGroup(gid int) (Policy, error) {
	if gid < 0 {
		return Policy{}, errors.New("trusted group requires a nonnegative gid")
	}
	return Policy{group: true, gid: gid}, nil
}
func (p Policy) Mode() os.FileMode {
	if p.group {
		return GroupMode
	}
	return OwnerMode
}
func (p Policy) GroupID() (int, bool) { return p.gid, p.group }

// Select supplies the private default and refuses contradictory policies.
func Select(policies ...Policy) (Policy, error) {
	if len(policies) > 1 {
		return Policy{}, errors.New("at most one sensitive file policy is allowed")
	}
	if len(policies) == 1 {
		return policies[0], nil
	}
	return Policy{}, nil
}

// Check verifies a stat result, including the expected group identity.
func (p Policy) Check(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("expected a regular file")
	}
	if info.Mode().Perm() != p.Mode() {
		return fmt.Errorf("mode %#o does not match private policy %#o", info.Mode().Perm(), p.Mode())
	}
	if p.group {
		gid, ok := groupID(info)
		if !ok || gid != p.gid {
			return fmt.Errorf("gid %d does not match trusted gid %d", gid, p.gid)
		}
	}
	return nil
}

// CheckDirectory protects native SQLite sidecar creation: a selected group
// must be the gid new files will inherit, and untrusted users cannot replace
// files through directory write access.
func (p Policy) CheckDirectory(info os.FileInfo) error {
	if !info.IsDir() {
		return errors.New("expected a database directory")
	}
	if !p.group {
		return nil
	}
	gid, ok := groupID(info)
	if !ok {
		return errors.New("cannot verify database directory gid")
	}
	if info.Mode().Perm()&0002 != 0 {
		return errors.New("database directory must not be writable by other users")
	}
	if gid != p.gid && info.Mode().Perm()&0020 != 0 {
		return errors.New("database directory is writable by an unauthorized group")
	}
	if info.Mode()&os.ModeSetgid != 0 {
		if gid != p.gid {
			return fmt.Errorf("database directory setgid %d differs from trusted gid %d", gid, p.gid)
		}
	} else if os.Getegid() != p.gid {
		return fmt.Errorf("database sidecars would inherit process gid %d, not trusted gid %d; use the installed service group or a correctly setgid directory", os.Getegid(), p.gid)
	}
	return nil
}

// Apply secures an already-open regular file and verifies its actual gid.
func (p Policy) Apply(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("expected a regular file")
	}
	if p.group {
		if err := removeAccessACL(f); err != nil {
			return err
		}
		info, err = f.Stat()
		if err != nil {
			return err
		}
		gid, ok := groupID(info)
		if !ok {
			return errors.New("group policy is unsupported on this filesystem")
		}
		if gid != p.gid {
			// Never grant a foreign setgid directory's group access, even briefly.
			if info.Mode().Perm() != OwnerMode {
				if err := f.Chmod(OwnerMode); err != nil {
					return err
				}
			}
			if err := f.Chown(-1, p.gid); err != nil {
				return fmt.Errorf("bind trusted gid %d: %w", p.gid, err)
			}
			info, err = f.Stat()
			if err != nil {
				return err
			}
			gid, ok = groupID(info)
			if !ok || gid != p.gid {
				return errors.New("file did not acquire its trusted gid")
			}
		}
	}
	if info.Mode().Perm() != p.Mode() {
		if err := f.Chmod(p.Mode()); err != nil {
			return err
		}
	}
	info, err = f.Stat()
	if err != nil {
		return err
	}
	return p.Check(info)
}

// Open refuses links and replacement between lookup and open. Files already
// matching their policy need no chmod, allowing a nonowner trusted operator.
func Open(path string, policies ...Policy) (*os.File, error) {
	policy, err := Select(policies...)
	if err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("protect %s: expected a regular file, not a symlink or special file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err == nil && !os.SameFile(before, after) {
		err = errors.New("file changed while it was being opened")
	}
	if err == nil {
		err = policy.Apply(f)
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("protect %s: %w; repair its owner/trusted-group permissions before retrying", path, err)
	}
	return f, nil
}
func Protect(path string, policies ...Policy) error {
	f, err := Open(path, policies...)
	if err != nil {
		return err
	}
	return f.Close()
}

// Verify checks a file against its policy without opening or changing it, for
// callers that may not read the file but must still confirm it is protected.
func Verify(path string, policies ...Policy) error {
	policy, err := Select(policies...)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := policy.Check(info); err != nil {
		return fmt.Errorf("protect %s: %w", path, err)
	}
	return CheckFileACL(path)
}
func ReadFile(path string, policies ...Policy) ([]byte, error) {
	f, err := Open(path, policies...)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Create always starts owner-only, binds an explicit gid if needed, and only
// then exposes the file to that group. It never replaces an existing path.
func Create(path string, policies ...Policy) (*os.File, error) {
	policy, err := Select(policies...)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, OwnerMode)
	if err != nil {
		return nil, err
	}
	if err := policy.Apply(f); err != nil {
		f.Close()
		os.Remove(path)
		return nil, fmt.Errorf("protect %s: %w", path, err)
	}
	return f, nil
}
