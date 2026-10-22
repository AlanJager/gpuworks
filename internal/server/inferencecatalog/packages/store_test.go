package packages

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestNewRejectsBroadRootAndAssetPathEscapes(t *testing.T) {
	if _, err := New("."); err == nil {
		t.Fatal("broad package root was accepted")
	}
	store, err := New(t.TempDir() + "/packages")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadAsset("bad", "org/model", "assets/a.py"); err == nil {
		t.Fatal("invalid digest was accepted")
	}
	if _, err := store.ReadAsset("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "org/model", "../a.py"); err == nil {
		t.Fatal("escaping asset was accepted")
	}
	if _, _, err := store.Put(fstest.MapFS{"unexpected": {Data: []byte("x")}}); err == nil {
		t.Fatal("invalid package was persisted")
	}
}

func TestPruneRemovesOnlyUnreferencedDigestDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "packages")
	store, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := strings.Repeat("a", 64)
	removedA := strings.Repeat("b", 64)
	removedB := strings.Repeat("c", 64)
	for _, name := range []string{kept, removedA, removedB, ".package-staging", "not-a-digest"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	digestFile := strings.Repeat("d", 64)
	if err := os.WriteFile(filepath.Join(root, digestFile), []byte("not a package directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := store.Prune([]string{" " + strings.ToUpper(kept) + " ", "invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{removedA, removedB}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed=%v want=%v", got, want)
	}
	for _, name := range []string{kept, ".package-staging", "not-a-digest", digestFile} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Fatalf("non-eligible entry %q was removed: %v", name, err)
		}
	}
}
