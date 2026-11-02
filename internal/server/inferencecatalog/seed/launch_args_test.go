package seed

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// TestEveryEmbeddedSpecArgvDecodesAgainstTheDictionary sweeps the whole
// built-in catalog through the lint behind CATALOG_ARG_PASSED_THROUGH. An
// import only warns, but a built-in spec the dictionary cannot read means the
// dictionary has fallen behind the engines the platform ships: refresh it
// (internal/server/deploy/param_specs.go) rather than change the recipe.
func TestEveryEmbeddedSpecArgvDecodesAgainstTheDictionary(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	models, specs, judged := len(catalog.Models), 0, 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			specs++
			// Counting the specs the dictionary can judge at all keeps this
			// sweep from going green because every spec became opaque to it.
			if slices.Contains(engines.DialectNames(), spec.Engine) {
				judged++
			}
			for _, problem := range deploy.LintArgs(spec.Engine, spec.Args) {
				t.Errorf("%s/%s: %s", model.ModelID, spec.Name, problem)
			}
		}
	}
	t.Logf("scanned %d models, %d specs, %d of them on an engine with a parameter dictionary", models, specs, judged)
	if models < 300 || specs < 900 || judged < 900 {
		t.Fatalf("sweep covered %d models / %d specs / %d dictionary-engine specs, too little to prove anything", models, specs, judged)
	}
}

// TestEmbeddedCatalogWarnsAboutAnUndecodableSpecArgv proves the sweep above
// is not green because the lint is inert: the same catalog, with one spec's
// --max-model-len corrupted, still validates (a template is never blocked by
// the dictionary) but reports the flag as passed through.
func TestEmbeddedCatalogWarnsAboutAnUndecodableSpecArgv(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	modelID, specName := corruptFirstVLLMSpec(t, &catalog)
	if err := inferencecatalog.ValidateFlatCatalog(&catalog); err != nil {
		t.Fatalf("catalog with --max-model-len abc was refused: %v", err)
	}
	found := false
	for _, warning := range inferencecatalog.CatalogWarnings(catalog) {
		params := warning.Params()
		if warning.Code() == "CATALOG_ARG_PASSED_THROUGH" && params["model_id"] == modelID && params["spec"] == specName && params["flag"] == "--max-model-len" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no CATALOG_ARG_PASSED_THROUGH warning for %s/%s", modelID, specName)
	}
}

func corruptFirstVLLMSpec(t *testing.T, catalog *inferencecatalog.FlatCatalog) (modelID, specName string) {
	t.Helper()
	for mi := range catalog.Models {
		for si := range catalog.Models[mi].Specs {
			spec := &catalog.Models[mi].Specs[si]
			if spec.Engine != engines.VLLM {
				continue
			}
			spec.Args = append([]string{"--max-model-len", "abc"}, spec.Args...)
			return catalog.Models[mi].ModelID, spec.Name
		}
	}
	t.Fatal("embedded catalog has no vLLM spec")
	return "", ""
}
