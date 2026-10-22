package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyCanonicalPathIncludesUncreatedParents(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got, err := copyCanonicalPath(filepath.Join(alias, "incoming", "model"))
	if err != nil || got != filepath.Join(real, "incoming", "model") {
		t.Fatalf("%s %v", got, err)
	}
}
