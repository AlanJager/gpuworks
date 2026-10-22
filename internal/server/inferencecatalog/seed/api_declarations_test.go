package seed

import (
	"testing"
)

// undeclaredAPI lists the catalog models whose specs deliberately declare no
// inference API, with the reason. It must stay short: a spec without
// api.operations deploys into a model record with no operations, so the
// Playground refuses every request to it and the health check has nothing to
// ask (inferenceapi.EffectiveContract no longer derives a default). Add an
// entry only for a model whose wire protocol cannot be confirmed, never to
// skip the declaration of an ordinary chat, text completion, embedding,
// rerank, audio, image or video model.
var undeclaredAPI = map[string]string{}

// Every recipe must say which HTTP operations its deployment serves. Without
// the declaration a deployment created from the template exposes nothing and
// the Playground answers "does not expose POST /v1/chat/completions" (T73).
func TestEveryEmbeddedSpecDeclaresItsAPIOperations(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			declared := spec.API != nil && len(spec.API.Operations) > 0
			reason, allowed := undeclaredAPI[model.ModelID]
			switch {
			case allowed:
				seen[model.ModelID] = true
				if declared {
					t.Errorf("%s/%s declares an API but %s is allowlisted as undeclared (%s); remove it from undeclaredAPI", model.ModelID, spec.Name, model.ModelID, reason)
				}
			case !declared:
				t.Errorf("%s/%s declares no api.operations, so a deployment from it would expose no inference API", model.ModelID, spec.Name)
			}
		}
	}
	for modelID := range undeclaredAPI {
		if !seen[modelID] {
			t.Errorf("undeclaredAPI lists %s, which is not in the embedded catalog; remove the stale entry", modelID)
		}
	}
}
