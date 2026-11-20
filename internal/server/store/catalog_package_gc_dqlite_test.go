//go:build linux && dqlite

package store

import (
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// The clustered store carries the barrier from its first start, advances it
// through the local scheduling connection on each garbage collection read,
// and keeps the committed sequence across a restart.
func TestNativeCatalogPackageGCCommitsTheBarrier(t *testing.T) {
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "gpuworks.db")
	st, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var marker SchemaMigration
	if err := st.DB().WithContext(ctx).Where("version = ?", 245).First(&marker).Error; err != nil || marker.Name != "catalog_gc_barrier" {
		t.Fatalf("marker = %+v err = %v, want version 245 catalog_gc_barrier", marker, err)
	}
	if got := catalogGCSequence(t, st); got != 0 {
		t.Fatalf("fresh barrier sequence = %d, want 0", got)
	}
	for _, want := range []int64{1, 2} {
		references, err := st.CatalogPackageReferencesForGC(ctx)
		if err != nil || len(references) != 0 {
			t.Fatalf("references = %v err = %v, want an empty committed list", references, err)
		}
		if got := catalogGCSequence(t, st); got != want {
			t.Fatalf("barrier sequence = %d, want %d", got, want)
		}
	}
	if err := st.DB().WithContext(ctx).Exec(`INSERT INTO instances (uuid, spec) VALUES ('broken', '{invalid')`).Error; err != nil {
		t.Fatal(err)
	}
	if references, err := st.CatalogPackageReferencesForGC(ctx); err == nil || references != nil {
		t.Fatalf("unreadable reference returned %v, %v; want a refusal", references, err)
	}
	if got := catalogGCSequence(t, st); got != 2 {
		t.Fatalf("a refused read moved the barrier to %d", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRuntimeWith(ctx, path, bus.New(), OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if got := catalogGCSequence(t, reopened); got != 2 {
		t.Fatalf("barrier sequence after restart = %d, want 2", got)
	}
}
