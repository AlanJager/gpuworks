//go:build linux

package securefile

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const accessACL = "system.posix_acl_access"
const defaultACL = "system.posix_acl_default"

func hasACL(f *os.File, attribute string) (bool, error) {
	_, err := unix.Fgetxattr(int(f.Fd()), attribute, nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
		// A missing ACL, or a filesystem without POSIX ACL support, cannot grant
		// named users access in addition to the mode and trusted group.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s ACL: %w", f.Name(), err)
	}
	return true, nil
}

func removeAccessACL(f *os.File) error {
	present, err := hasACL(f, accessACL)
	if err != nil || !present {
		return err
	}
	// chmod changes the ACL mask, not just the owning group's permissions.
	// Revoke named-user access before removal, then reaffirm private mode in
	// case removing the ACL changes how group_obj permissions are represented.
	if err := f.Chmod(OwnerMode); err != nil {
		return fmt.Errorf("revoke extended ACL on %s: %w", f.Name(), err)
	}
	if err := unix.Fremovexattr(int(f.Fd()), accessACL); err != nil && !errors.Is(err, unix.ENODATA) {
		return fmt.Errorf("remove extended ACL on %s: %w", f.Name(), err)
	}
	if err := f.Chmod(OwnerMode); err != nil {
		return err
	}
	if present, err := hasACL(f, accessACL); err != nil {
		return err
	} else if present {
		return fmt.Errorf("extended ACL remains on %s", f.Name())
	}
	return nil
}

// CheckDirectoryACL rejects extended access and inherited default ACLs. It
// never changes a parent directory: native SQLite sidecars and privileged
// temporary files must not inherit grants to users outside the trusted group.
func CheckDirectoryACL(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("check ACL %s: expected a directory", f.Name())
	}
	for _, attribute := range []string{accessACL, defaultACL} {
		present, err := hasACL(f, attribute)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("directory %s has an extended access or default ACL; have its owner remove unexpected ACL grants before retrying", f.Name())
		}
	}
	return nil
}

// CheckFileACL inspects permission metadata without opening file contents. It
// guards recursive group chmod against reactivating a masked named-user ACL;
// Policy.Apply remains responsible for descriptor-based permission changes.
func CheckFileACL(path string) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("check ACL %s: expected a regular file, not a symlink or special file", path)
	}
	_, aclErr := unix.Lgetxattr(path, accessACL, nil)
	after, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return fmt.Errorf("check ACL %s: file changed during metadata inspection", path)
	}
	if errors.Is(aclErr, unix.ENODATA) || errors.Is(aclErr, unix.EOPNOTSUPP) {
		return nil
	}
	if aclErr != nil {
		return fmt.Errorf("inspect %s ACL: %w", path, aclErr)
	}
	return fmt.Errorf("file %s has an extended access ACL; have its owner remove unexpected ACL grants before retrying", path)
}
