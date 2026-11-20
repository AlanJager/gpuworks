//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func activationPrivateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAddressActivationDigestBindsWholeTree(t *testing.T) {
	dir := activationPrivateTestDir(t)
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := func() string {
		t.Helper()
		value, err := addressActivationDigest(t.Context(), dir)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	original := digest()
	if len(original) != 64 || digest() != original {
		t.Fatal("digest is not deterministic SHA256")
	}
	for _, change := range []struct {
		name    string
		apply   func() error
		restore func() error
	}{
		{"contents", func() error { return os.WriteFile(path, []byte("modified"), 0600) }, func() error { return os.WriteFile(path, []byte("original"), 0600) }},
		{"file_mode", func() error { return os.Chmod(path, 0400) }, func() error { return os.Chmod(path, 0600) }},
		{"filename", func() error { return os.Rename(path, path+"-new") }, func() error { return os.Rename(path+"-new", path) }},
		{"directory", func() error { return os.Mkdir(filepath.Join(dir, "empty"), 0700) }, func() error { return os.Remove(filepath.Join(dir, "empty")) }},
		{"root_mode", func() error { return os.Chmod(dir, 0500) }, func() error { return os.Chmod(dir, 0700) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			if err := change.apply(); err != nil {
				t.Fatal(err)
			}
			if digest() == original {
				t.Fatal("filesystem change omitted from digest")
			}
			if err := change.restore(); err != nil {
				t.Fatal(err)
			}
			if digest() != original {
				t.Fatal("restoring content did not restore digest")
			}
		})
	}
	transport := filepath.Join(dir, ".transport")
	if err := os.Mkdir(transport, 0700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(transport, "policy.lock")
	if err := os.WriteFile(lock, nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := digest()
	if err := os.WriteFile(lock, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if digest() == before {
		t.Fatal("transport policy lock excluded")
	}
}

func TestAddressActivationDigestRejectsUnsafeTreeAndCancellation(t *testing.T) {
	dir := activationPrivateTestDir(t)
	outside := activationPrivateTestDir(t)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := addressActivationDigest(t.Context(), dir); err == nil {
		t.Fatal("accepted symlink entry")
	}
	if _, err := addressActivationDigest(t.Context(), link); err == nil {
		t.Fatal("accepted symlink root")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := addressActivationDigest(t.Context(), dir); err == nil {
		t.Fatal("accepted public root")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := addressActivationDigest(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

type activationFileRecord struct {
	Name     string `json:"name"`
	Revision int    `json:"revision"`
}

func TestAddressActivationPublicationIsImmutable(t *testing.T) {
	dir := activationPrivateTestDir(t)
	path := filepath.Join(dir, "activation.json")
	value := activationFileRecord{"member", 1}
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() { failures <- writeAddressActivation(path, value) })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode=%v", info.Mode())
	}
	var got activationFileRecord
	if err := readAddressActivation(path, &got); err != nil || got != value {
		t.Fatalf("round trip=%+v %v", got, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAddressActivation(path, activationFileRecord{"member", 2}); err == nil {
		t.Fatal("overwrote different publication")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("failed publication damaged original")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files leaked: %v %v", entries, err)
	}
}

func TestAddressActivationReadAndRetryRejectDamage(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"unknown", `{"name":"member","revision":1,"extra":1}`, 0600},
		{"trailing", `{"name":"member","revision":1} {}`, 0600},
		{"truncated", `{"name":`, 0600},
		{"empty", "", 0600},
		{"oversized", strings.Repeat(" ", addressActivationFileLimit+1), 0600},
		{"public", `{"name":"member","revision":1}`, 0644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(activationPrivateTestDir(t), "record")
			if err := os.WriteFile(path, []byte(tc.body), tc.mode); err != nil {
				t.Fatal(err)
			}
			var got activationFileRecord
			if err := readAddressActivation(path, &got); err == nil {
				t.Fatal("accepted damaged record")
			}
			if err := writeAddressActivation(path, activationFileRecord{"member", 1}); err == nil {
				t.Fatal("replaced damaged record")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != tc.body {
				t.Fatal("damaged original mutated")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		dir := activationPrivateTestDir(t)
		target := filepath.Join(dir, "target")
		if err := writeAddressActivation(target, activationFileRecord{"member", 1}); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "link")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		var got activationFileRecord
		if err := readAddressActivation(path, &got); err == nil {
			t.Fatal("read followed symlink")
		}
		if err := writeAddressActivation(path, activationFileRecord{"member", 1}); err == nil {
			t.Fatal("retry followed symlink")
		}
	})
	t.Run("directory", func(t *testing.T) {
		path := activationPrivateTestDir(t)
		var got activationFileRecord
		if err := readAddressActivation(path, &got); err == nil {
			t.Fatal("read directory")
		}
		if err := writeAddressActivation(path, activationFileRecord{}); err == nil {
			t.Fatal("replaced directory")
		}
	})
}

func TestAddressActivationPublicationParentPermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0750, 0755, 0770, 0777} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := activationPrivateTestDir(t)
			if err := os.Chmod(dir, mode); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "record")
			err := writeAddressActivation(path, activationFileRecord{"member", 1})
			if mode&0022 != 0 {
				if err == nil {
					t.Fatal("accepted writable parent")
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected parent has publication: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 {
					t.Fatalf("record mode=%v", info.Mode())
				}
			}
			info, err := os.Lstat(dir)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("parent mode changed: %v", err)
			}
		})
	}
}
