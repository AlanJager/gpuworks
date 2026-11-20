package packages

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"
)

// A cluster member verifies the catalog packages the package store holds. An
// official package is stored as published and may carry a model a newer
// server wrote; the store loads it, so the member's check must as well, or a
// member could never prove the materials of a cluster that imported one.
func TestPackageRootContextReadsAnOfficialPackageAsTheStoreDoes(t *testing.T) {
	source := manyModelPackageFixture(t, 2)
	const written = "models/org/model-0001/model.yaml"
	newer := append([]byte("model_field_of_a_newer_server: 1\n"), source[written].Data...)
	source[written] = &fstest.MapFile{Data: newer}

	dir := filepath.Join(t.TempDir(), "packages")
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, skipped, digest, _, err := store.PutOfficial(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) == 0 {
		t.Fatal("the fixture holds nothing this build skips")
	}
	want, err := store.Load(digest)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got, err := LoadRootContext(t.Context(), root, digest)
	if err != nil {
		t.Fatalf("the member's check refused a package the store loads: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the member's projection differs from the store's: %+v, want %+v", got, want)
	}
}
