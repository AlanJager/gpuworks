//go:build linux

package securefile

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// Linux POSIX ACL wire format. The named UID deliberately has no relationship
// to the authorized file gid; chmod's group bits represent the ACL mask.
func namedUserACL() []byte {
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{
		{1, 7, ^uint32(0)}, {2, 6, 65534}, {4, 7, ^uint32(0)}, {16, 6, ^uint32(0)}, {32, 0, ^uint32(0)},
	}
	data := make([]byte, 4+8*len(entries))
	binary.LittleEndian.PutUint32(data, 2)
	for i, e := range entries {
		off := 4 + 8*i
		binary.LittleEndian.PutUint16(data[off:], e.tag)
		binary.LittleEndian.PutUint16(data[off+2:], e.perm)
		binary.LittleEndian.PutUint32(data[off+4:], e.id)
	}
	return data
}

func TestGroupCreationRemovesInheritedAccessACL(t *testing.T) {
	dir := t.TempDir()
	if err := unix.Setxattr(dir, "system.posix_acl_default", namedUserACL(), 0); err != nil {
		t.Fatal(err)
	}
	policy, err := TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "new-secret")
	f, err := Create(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString("synthetic secret"); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.Fgetxattr(int(f.Fd()), "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("new group file retained named-user ACL: %v", err)
	}
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err = policy.Check(info); err != nil {
		t.Fatal(err)
	}
}

func TestGroupProtectRemovesExistingAccessACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing-secret")
	if err := os.WriteFile(path, []byte("same synthetic secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, "system.posix_acl_access", namedUserACL(), 0); err != nil {
		t.Fatal(err)
	}
	policy, err := TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if err = Protect(path, policy); err != nil {
		t.Fatal(err)
	}
	if _, err = unix.Getxattr(path, "system.posix_acl_access", nil); !errors.Is(err, unix.ENODATA) {
		t.Fatalf("existing group file retained named-user ACL: %v", err)
	}
	data, err := ReadFile(path, policy)
	if err != nil || string(data) != "same synthetic secret" {
		t.Fatalf("file changed: %q %v", data, err)
	}
}

func TestDirectoryACLCheckRefusesInvalidDescriptorsAndRegularFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular-file")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDirectoryACL(f); err == nil {
		t.Fatal("regular file accepted as a trusted parent")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckDirectoryACL(f); err == nil {
		t.Fatal("closed parent descriptor accepted")
	}
	if err := removeAccessACL(f); err == nil {
		t.Fatal("failed ACL inspection was ignored")
	}
}

func TestFileACLInspectionRejectsMaskedACLAndLinksWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "private-secret")
	if err := os.WriteFile(path, []byte("untouched"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileACL(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Setxattr(path, accessACL, namedUserACL(), 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFileACL(path); err == nil {
		t.Fatal("masked named-user ACL could be reactivated by group chmod")
	}
	if _, err := unix.Getxattr(path, accessACL, nil); err != nil {
		t.Fatalf("read-only inspection modified ACL: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("read-only inspection changed private mode")
	}
	link := filepath.Join(dir, "linked-secret")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{link, dir} {
		if err := CheckFileACL(name); err == nil {
			t.Fatalf("nonregular file accepted: %s", name)
		}
	}
	if err := CheckFileACL(filepath.Join(dir, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-file error=%v", err)
	}
}
