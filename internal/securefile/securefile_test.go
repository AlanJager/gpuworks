package securefile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateReadAndProtectKeepSensitiveFilesInTrustedGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	f, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("unchanged secret"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite existing file = %v", err)
	}
	if err := os.Chmod(path, 0664); err != nil {
		t.Fatal(err)
	}
	b, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "unchanged secret" {
		t.Fatal("contents changed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != OwnerMode {
		t.Fatalf("mode=%#o", info.Mode().Perm())
	}
}

func TestReadRefusesLinksAndDirectoriesWithoutChangingTheirTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(target, []byte("do not read"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{link, dir} {
		if _, err := ReadFile(path); err == nil {
			t.Fatalf("nonregular path read: %s", path)
		}
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("symlink target was changed: %#o", info.Mode().Perm())
	}
	if _, err := Create(link); !errors.Is(err, os.ErrExist) {
		t.Fatalf("creation replaced symlink: %v", err)
	}
}

func TestPoliciesOnlyExplicitlyGrantTheBoundGroup(t *testing.T) {
	if _, err := TrustedGroup(-1); err == nil {
		t.Fatal("negative gid accepted")
	}
	policy, err := TrustedGroup(os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "group-secret")
	f, err := Create(path, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.Check(info); err != nil {
		t.Fatal(err)
	}
	if err := Protect(path); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != OwnerMode {
		t.Fatal("default retained unproven group access")
	}
	if _, err := Open(path, Policy{}, policy); err == nil {
		t.Fatal("multiple policies accepted")
	}
}

func TestUnbindableGroupCannotExposeNewOrExistingFiles(t *testing.T) {
	gid := 65533
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	for {
		present := gid == os.Getgid()
		for _, g := range groups {
			present = present || g == gid
		}
		if !present {
			break
		}
		gid--
	}
	policy, err := TrustedGroup(gid)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "new-secret")
	f, err := Create(path, policy)
	if os.Geteuid() == 0 {
		if err != nil {
			t.Fatal(err)
		}
		info, _ := f.Stat()
		f.Close()
		if err := policy.Check(info); err != nil {
			t.Fatal(err)
		}
	} else {
		if err == nil {
			f.Close()
			t.Fatal("nonmember bound an unauthorized gid")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed creation left a file")
		}
	}
	existing := filepath.Join(t.TempDir(), "existing-secret")
	if err := os.WriteFile(existing, []byte("same secret"), 0664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(existing, 0664); err != nil {
		t.Fatal(err)
	}
	_, err = ReadFile(existing, policy)
	if os.Geteuid() != 0 {
		if err == nil {
			t.Fatal("wrong gid silently granted")
		}
		info, _ := os.Stat(existing)
		if info.Mode().Perm() != OwnerMode {
			t.Fatalf("failed bind left broad mode %#o", info.Mode().Perm())
		}
	}
	data, _ := os.ReadFile(existing)
	if string(data) != "same secret" {
		t.Fatal("failed bind changed bytes")
	}
}
