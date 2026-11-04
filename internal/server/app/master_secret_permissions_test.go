package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMasterSecretRestrictsNewAndExistingKeys(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "master.key")
			if existing {
				if err := os.WriteFile(path, []byte("existing-secret"), 0664); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0664); err != nil {
					t.Fatal(err)
				}
			}
			secret, secretErr := masterSecret(Config{DataDir: dir})
			if secretErr != nil {
				t.Fatal(secretErr)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0600 {
				t.Fatalf("key mode=%#o, want0600", info.Mode().Perm())
			}
			if existing && secret != "existing-secret" {
				t.Fatal("existing key rotated")
			}
		})
	}
}

func TestMasterSecretRefusesUnreadableOrInvalidExistingKeyWithoutReplacingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, nil, 0664); err != nil {
		t.Fatal(err)
	}
	if _, err := masterSecret(Config{DataDir: dir}); err == nil {
		t.Fatal("empty existing key was replaced")
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Fatalf("empty original changed: %d/%v", len(b), err)
	}
	os.Remove(path)
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, []byte("target-secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := masterSecret(Config{DataDir: dir}); err == nil {
		t.Fatal("symlink secret accepted")
	}
	if b, _ := os.ReadFile(target); string(b) != "target-secret" {
		t.Fatal("target contents changed")
	}
	if _, err := masterSecret(Config{DataDir: filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("unpersisted random key returned")
	}
}
