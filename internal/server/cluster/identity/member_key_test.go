package identity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMemberKeyPublicationAndConcurrentCreators(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "member.key")
	type result struct {
		key []byte
		err error
	}
	results := make(chan result, 8)
	var start sync.WaitGroup
	start.Add(1)
	for range 8 {
		go func() { start.Wait(); key, err := CreateMemberKey(path); results <- result{key, err} }()
	}
	start.Done()
	var winner []byte
	for range 8 {
		result := <-results
		if result.err == nil {
			if winner != nil {
				t.Fatal("two published identities")
			}
			winner = result.key
		} else if !errors.Is(result.err, os.ErrExist) {
			t.Fatal(result.err)
		}
	}
	if winner == nil {
		t.Fatal("no creator succeeded")
	}
	loaded, err := LoadMemberKey(path)
	if err != nil || !bytes.Equal(winner, loaded) {
		t.Fatalf("identity changed: %v", err)
	}
	csr, err := NewMemberCSR(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CSRKeyFingerprint(csr); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("temporary files left: %v %v", entries, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("key mode: %v %v", info, err)
	}
}

func TestMemberKeyLoadNeverRepairsInvalidState(t *testing.T) {
	for _, kind := range []string{"missing", "partial", "public", "symlink", "public-parent"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "member.key")
			var original []byte
			if kind != "missing" {
				var err error
				original, err = CreateMemberKey(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "partial":
				original = []byte("incomplete")
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				other := filepath.Join(dir, "original.key")
				if err := os.Rename(path, other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			case "public-parent":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := LoadMemberKey(path); err == nil {
				t.Fatal("invalid state accepted")
			}
			if kind == "missing" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("missing key recreated: %v", err)
				}
				return
			}
			if _, err := CreateMemberKey(path); err == nil {
				t.Fatal("existing identity replaced")
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, actual) {
				t.Fatalf("source changed: %v", err)
			}
		})
	}
}
