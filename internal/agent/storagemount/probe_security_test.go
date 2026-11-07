package storagemount

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent refuses malformed paths and symbolic-link aliases on its own; the
// operating-system denylist is server policy (storagerules.ValidatePath), so a
// server that replaced that list can place storage under a former default.
func TestStorageProbeRejectsMalformedPathsAndSymlinkAliases(t *testing.T) {
	m := New(Options{})
	for _, p := range []string{"/", "relative/models", "/data/../etc", "/data/./models", "/data/\x00models"} {
		res := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: p})
		if res.OK || res.Readable || res.Writable {
			t.Fatalf("malformed probe succeeded: %q %+v", p, res)
		}
	}
	dir := t.TempDir()
	alias := filepath.Join(dir, "models")
	if err := os.Symlink("/etc", alias); err != nil {
		t.Fatal(err)
	}
	cacheAlias := filepath.Join(dir, "cache")
	if err := os.Symlink("/etc", cacheAlias); err != nil {
		t.Fatal(err)
	}
	res := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: alias})
	if res.OK || res.Readable || res.Writable || !strings.Contains(res.Message, "canonical") {
		t.Fatalf("symlink alias probe: %+v", res)
	}
	statuses := m.Reconcile(context.Background(), []Spec{{ID: 1, Type: TypeLocal, Path: alias}, {ID: 2, Type: TypeNFSCache, Path: dir, CachePath: cacheAlias, Export: "nfs:/models"}})
	for _, status := range statuses {
		if status.State == StateReady || status.Writable || !strings.Contains(status.Message, "canonical") {
			t.Fatalf("unsafe storage reconciled: %+v", status)
		}
	}
}

func TestStoragePathsRejectSymlinkBeforeParentTraversal(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "allowed"), filepath.Join(base, "outside")
	for _, dir := range []string{root, filepath.Join(outside, "anchor"), filepath.Join(outside, "secret")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "anchor"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// Concatenation preserves the raw traversal, which filepath.Join would remove.
	raw := root + "/link/../secret"
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil || resolved != filepath.Join(outside, "secret") {
		t.Fatalf("fixture did not escape the allowed root: %s %v", resolved, err)
	}
	statCalls := 0
	m := New(Options{Statfs: func(string) (Usage, error) {
		statCalls++
		return Usage{TotalBytes: 100, AvailableBytes: 50}, nil
	}})
	res := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: raw})
	if res.OK || res.Readable || res.Writable || statCalls != 0 {
		t.Errorf("raw traversal probed outside the root: %+v statfs calls=%d", res, statCalls)
	}
	statuses := m.Reconcile(context.Background(), []Spec{{ID: 1, Type: TypeLocal, Path: raw}})
	if statuses[0].State == StateReady || statuses[0].Writable || statCalls != 0 {
		t.Errorf("raw traversal reconciled outside the root: %+v statfs calls=%d", statuses, statCalls)
	}
}

func TestStorageProbeMissingTopLevelDirectoryUsesExistingAncestor(t *testing.T) {
	// No filesystem mutation is needed to probe a local directory that the
	// agent will create later. The existing ancestor may be the filesystem root.
	top := string(filepath.Separator) + filepath.Base(filepath.Dir(t.TempDir())) + "-not-created"
	p := filepath.Join(top, "models")
	if _, err := os.Lstat(top); !os.IsNotExist(err) {
		t.Fatalf("fixture top-level directory must not exist: %v", err)
	}
	m := New(Options{})
	res := m.Probe(context.Background(), Spec{Type: TypeLocal, Path: p})
	if !res.OK || !res.Readable {
		t.Fatalf("allowed uncreated local directory rejected: %+v", res)
	}
}
