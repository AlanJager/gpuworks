package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeMountManagerPreparesTraversableReadOnlyAssetPath(t *testing.T) {
	state := t.TempDir()
	t.Setenv(runtimeMountRootEnv, filepath.Join(state, "runtime-mounts"))
	manager := NewRuntimeMountManager(state, nil)
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	owner, destination, err := manager.assetPath(digest, ".file")
	if err != nil {
		t.Fatal(err)
	}
	assetsDir := filepath.Dir(destination)
	for _, name := range []string{manager.Root(), assetsDir} {
		info, statErr := os.Stat(name)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != 0o755 {
			t.Fatalf("%s mode=%o, want 0755", name, info.Mode().Perm())
		}
	}
	if owner.UID != os.Geteuid() || owner.GID != os.Getegid() {
		t.Fatalf("owner=%+v, want current uid/gid", owner)
	}
	if err := os.WriteFile(destination, []byte("asset"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := manager.finalizeReadOnly(destination, owner); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("asset mode=%o, want 0444", info.Mode().Perm())
	}
}

func TestRuntimeMountManagerRejectsSymlinkAndUnsafePurposeKey(t *testing.T) {
	state := t.TempDir()
	manager := NewRuntimeMountManager(state, nil)
	if _, err := manager.PrepareInstanceDir("future-b", "x"); err == nil {
		t.Fatal("unallowlisted purpose accepted")
	}
	if _, err := manager.PrepareInstanceDir("overlays", "../escape"); err == nil {
		t.Fatal("path traversal key accepted")
	}
	if err := os.MkdirAll(manager.Root(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(manager.Root(), "overlays")); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.PrepareInstanceDir("overlays", "instance"); err == nil {
		t.Fatal("symlink purpose directory accepted")
	}
}

func TestRuntimeMountManagerRepairsManagedRootModeOnly(t *testing.T) {
	state := t.TempDir()
	root := filepath.Join(state, "runtime-mounts")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := NewRuntimeMountManager(state, nil)
	if _, err := manager.PrepareInstanceDir("pre-start", "instance"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("managed root mode=%o, want 0755", info.Mode().Perm())
	}
	private := filepath.Join(state, "private")
	if err := os.WriteFile(private, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	privateInfo, err := os.Stat(private)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unmanaged state mode changed to %o", privateInfo.Mode().Perm())
	}
}

func TestRuntimeMountManagerPreStartPublicationIsAtomicAndIdempotent(t *testing.T) {
	state := t.TempDir()
	manager := NewRuntimeMountManager(state, nil)
	dir, err := manager.PrepareInstanceDir("pre-start", "instance-1")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := manager.mountOwner()
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.writeReadOnlyFileAtomic(dir, "pre-start.sh", []byte("#!/bin/sh\nexec \"$@\"\n"), owner)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.writeReadOnlyFileAtomic(dir, "pre-start.sh", []byte("#!/bin/sh\nexec \"$@\"\n"), owner)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(firstInfo, secondInfo) {
		t.Fatal("identical pre-start reconcile republished a valid file")
	}
	if secondInfo.Mode().Perm() != 0o444 {
		t.Fatalf("wrapper mode=%o, want 0444", secondInfo.Mode().Perm())
	}
}
