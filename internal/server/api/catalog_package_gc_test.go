package api

import (
	"io"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An empty, readable reference list is not permission to delete: the prune
// removes a package only after its barrier write committed.
func TestCatalogPackagePruneNeedsACommittedBarrier(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "catalog-gc.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	catalogPackages, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	model := inferencecatalog.InferenceModel{ModelID: "gpuworks/cleanup", Source: "user", Specs: []inferencecatalog.InferenceSpec{{
		Name: "default", Engine: "vllm", Image: "example/vllm:test",
		Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "gpuworks/cleanup"}}},
		GPUs:    inferencecatalog.GPUSelector{Vendor: "nvidia"},
	}}}
	packageFS, err := inferencecatalog.BuildPackageFS("2026-09-01T00:00:00Z", inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{model}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	digest, created, err := catalogPackages.Put(packageFS)
	if err != nil || !created {
		t.Fatalf("put digest=%s created=%v err=%v", digest, created, err)
	}
	handler := &apiHandlers{d: Deps{Store: database, CatalogPackages: catalogPackages}}
	if err := database.DB().Exec(`CREATE TRIGGER reject_gc BEFORE UPDATE ON catalog_gc_barriers BEGIN SELECT RAISE(ABORT, 'barrier unavailable'); END`).Error; err != nil {
		t.Fatal(err)
	}
	handler.pruneUnreferencedCatalogPackages()
	if err := catalogPackages.Pack(digest, io.Discard); err != nil {
		t.Fatalf("a prune without a committed barrier deleted the package: %v", err)
	}
	if err := database.DB().Exec("DROP TRIGGER reject_gc").Error; err != nil {
		t.Fatal(err)
	}
	handler.pruneUnreferencedCatalogPackages()
	if err := catalogPackages.Pack(digest, io.Discard); err == nil {
		t.Fatal("the unreferenced package survived a prune whose barrier committed")
	}
}
