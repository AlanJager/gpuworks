package storagemount

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSweepTrashRemovesOnlyAbandonedEntries(t *testing.T) {
	root := t.TempDir()
	now := time.Unix(1_800_000_000, 0)
	old := TrashPath(root, "Qwen/old-model", now.Add(-time.Hour))
	recent := TrashPath(root, "Qwen/recent-model", now.Add(-time.Minute))
	unstamped := filepath.Join(trashDir(root), "leftover")
	for _, dir := range []string{old, recent, unstamped} {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "sub", "weights.safetensors"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if filepath.Base(old) != "1799996400000000000-old-model" {
		t.Fatalf("trash name = %q", filepath.Base(old))
	}

	n, err := SweepTrash(root, 10*time.Minute, now)
	if err != nil || n != 2 {
		t.Fatalf("SweepTrash = %d, %v; want 2, nil", n, err)
	}
	for _, gone := range []string{old, unstamped} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s still exists: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(recent, "sub", "weights.safetensors")); err != nil {
		t.Fatalf("an entry still being deleted was swept: %v", err)
	}
}

func TestSweepTrashWithoutTrashDir(t *testing.T) {
	n, err := SweepTrash(t.TempDir(), time.Minute, time.Now())
	if err != nil || n != 0 {
		t.Fatalf("SweepTrash = %d, %v; want 0, nil", n, err)
	}
}

func TestSweepTrashReportsFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	entry := TrashPath(root, "m", time.Unix(0, 1))
	if err := os.MkdirAll(filepath.Join(entry, "locked", "f"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(entry, "locked"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(entry, "locked"), 0o755) })
	n, err := SweepTrash(root, time.Minute, time.Now())
	if err == nil || n != 0 {
		t.Fatalf("SweepTrash = %d, %v; want 0 and an error", n, err)
	}
}
