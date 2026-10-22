package seed

import (
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

// TestEveryEmbeddedSpecArgvDecodesAgainstTheDictionary sweeps the whole
// built-in catalog through the same lint the importer runs. Catalog() already
// refuses a package that fails it, so a regression would surface as a load
// error; sweeping here names every offending model, spec and flag instead of
// stopping at the first, which is what a catalog author needs in order to fix
// them.
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

// TestEmbeddedCatalogRejectsAnUndecodableSpecArgv proves the sweep above is not
// green because the lint is inert: the same catalog, with one spec's
// --max-model-len corrupted, fails validation.
func TestEmbeddedCatalogRejectsAnUndecodableSpecArgv(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	modelID, specName := corruptFirstVLLMSpec(t, &catalog)
	err = inferencecatalog.ValidateFlatCatalog(&catalog)
	if err == nil {
		t.Fatal("catalog with --max-model-len abc validated")
	}
	for _, want := range []string{modelID, specName, "--max-model-len", `"abc"`, "is not an integer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
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
