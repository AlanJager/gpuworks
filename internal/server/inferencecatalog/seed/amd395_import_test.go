package seed

import (
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testutil/catalogfixture"
)

func TestAMD395OverlayKeepsCurrentSeedFactsAndUpstreamOwnership(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "catalog.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatal(err)
	}
	before, err := st.InferenceModelByModelID("qwen/qwen3.8-27b")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	catalogfixture.WriteLaunchOverlay(t, "catalog", "../../../../build/inference-templates/amd395/catalog", root)
	pkg, err := inferencecatalog.LoadPackage(os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	models, err := inferencecatalog.ToFlatStoreModels(pkg.Catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog(models, false); err != nil {
		t.Fatal(err)
	}
	check := func() {
		model, err := st.InferenceModelByModelID("qwen/qwen3.8-27b")
		if err != nil {
			t.Fatal(err)
		}
		if model.Source != "upstream" || model.ModelConfig != before.ModelConfig || model.KVBytesPerToken != before.KVBytesPerToken {
			t.Fatalf("overlay changed canonical seed facts: source=%q", model.Source)
		}
		amd := 0
		for _, spec := range model.Specs {
			if spec.Source == "user" && (spec.Name == "bf16-vllm-amd395-gfx1151-local" || spec.Name == "bf16-sglang-amd395-gfx1151-local") {
				amd++
			}
		}
		if amd != 2 {
			t.Fatalf("AMD user specs imported = %d, want 2", amd)
		}
	}
	check()
	if err := reconcileEmbeddedCatalog(st); err != nil {
		t.Fatal(err)
	}
	check()
}
