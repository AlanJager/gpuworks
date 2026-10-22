//go:build linux || darwin

package expose

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreNeverChangesLinkedLockPermissions(t *testing.T) {
	for _, link := range []string{"symbolic", "hard"} {
		t.Run(link, func(t *testing.T) {
			store := NewStore(t.TempDir())
			if err := store.Ensure(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Root(), ".lock")
			var err error
			if link == "symbolic" {
				err = os.Symlink(target, path)
			} else {
				err = os.Link(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if release, err := store.Lock(time.Millisecond); err == nil {
				release()
				t.Fatal("linked lock accepted")
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("target permissions changed: %v %v", info, err)
			}
			raw, err := os.ReadFile(target)
			if err != nil || string(raw) != "secret" {
				t.Fatalf("target changed: %q %v", raw, err)
			}
		})
	}
}

func TestStoreEnsureNeverChangesLinkedDirectoryPermissions(t *testing.T) {
	for _, name := range []string{"logs", "credentials", "bin", "ancestor"} {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			store := NewStore(filepath.Join(parent, "data"))
			if err := store.Ensure(); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Root(), name)
			if name == "ancestor" {
				path = filepath.Join(parent, "data")
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if err := store.Ensure(); err == nil {
				t.Fatal("linked directory accepted")
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o700 {
				t.Fatalf("target permissions changed: %v %v", info, err)
			}
		})
	}
}

func TestAtomicStateWriteRejectsLinkedParent(t *testing.T) {
	target := t.TempDir()
	original := filepath.Join(target, "config.json")
	if err := os.WriteFile(original, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(link, "config.json"), []byte("replacement"), 0o664); err == nil {
		t.Fatal("linked parent accepted")
	}
	raw, err := os.ReadFile(original)
	info, statErr := os.Stat(original)
	if err != nil || statErr != nil || string(raw) != "secret" || info.Mode().Perm() != 0o600 {
		t.Fatalf("outside config changed: %q %v %v %v", raw, info, err, statErr)
	}
}
