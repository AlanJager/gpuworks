package packages

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

func TestLoadContextMatchesLoadAndStopsWhenCancelled(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	fsys, digest, err := seed.Package()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutLoaded(fsys, digest); err != nil {
		t.Fatal(err)
	}
	want, err := store.Load(digest)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.LoadContext(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || len(got.Catalog.Models) == 0 {
		t.Fatalf("LoadContext returned %d models, Load returned %d", len(got.Catalog.Models), len(want.Catalog.Models))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.LoadContext(ctx, digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled LoadContext error = %v, want context.Canceled", err)
	}
	if _, err := store.LoadContext(context.Background(), "not-a-digest"); err == nil {
		t.Fatal("LoadContext accepted an invalid digest")
	}
}
