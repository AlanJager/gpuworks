package inferencecatalog

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// weightRepoModel is the specs of catalog model id, one per weight_sources
// value.
func weightRepoModel(id uint, weightSources ...string) []store.InferenceSpecWeightSources {
	specs := make([]store.InferenceSpecWeightSources, 0, len(weightSources))
	for _, sources := range weightSources {
		specs = append(specs, store.InferenceSpecWeightSources{ModelID: id, WeightSources: sources})
	}
	return specs
}

func weightRepoSpecs(models ...[]store.InferenceSpecWeightSources) []store.InferenceSpecWeightSources {
	var specs []store.InferenceSpecWeightSources
	for _, model := range models {
		specs = append(specs, model...)
	}
	return specs
}

// The reverse lookup from a weight repository to the catalog model that
// declares it decodes the column with its owning type, so it does not depend on
// how the JSON happens to be serialised and cannot match across field
// boundaries the way a text search over the column could.
func TestModelDeclaringWeightRepo(t *testing.T) {
	base := weightRepoModel(7,
		`[{"provider":"huggingface","repo_id":"org/Base"},{"provider":"huggingface","repo_id":"org/Base-NVFP4"}]`)
	for _, tc := range []struct {
		name   string
		specs  []store.InferenceSpecWeightSources
		repo   string
		wantID uint // 0 = no match
	}{
		{
			name:   "case-insensitive and trimmed",
			specs:  base,
			repo:   "  ORG/Base-NVFP4\t",
			wantID: 7,
		},
		{
			name: "whitespace and newlines inside the JSON",
			specs: weightRepoModel(3,
				"[ {\n  \"provider\" : \"huggingface\",\n  \"repo_id\" : \"org/Base\"\n} ]"),
			repo:   "org/Base",
			wantID: 3,
		},
		{name: "LIKE wildcard is literal", specs: base, repo: "org/Base%"},
		{name: "prefix is not a match", specs: base, repo: "org/Bas"},
		{name: "longer name is not a match", specs: base, repo: "org/Base-NVFP4-extra"},
		{name: "undeclared repository", specs: base, repo: "org/Not-Declared-Anywhere"},
		{
			name: "revision and subfolder are not repository names",
			specs: weightRepoModel(4,
				`[{"provider":"huggingface","repo_id":"org/Other","revision":"org/Base-NVFP4"}]`,
				`[{"provider":"huggingface","repo_id":"org/Other","subfolder":"org/Base-NVFP4"}]`),
			repo: "org/Base-NVFP4",
		},
		{
			name: "malformed spec is skipped, a later spec of the same model still matches",
			specs: weightRepoModel(5,
				`not json`, ``, `{"repo_id":"org/Base-NVFP4"}`,
				`[{"provider":"huggingface","repo_id":"org/Base-NVFP4"}]`),
			repo:   "org/Base-NVFP4",
			wantID: 5,
		},
		{
			name: "malformed model is skipped, a later model still matches",
			specs: weightRepoSpecs(
				weightRepoModel(1, `[{"repo_id":`),
				weightRepoModel(2, `[{"provider":"huggingface","repo_id":"org/Base-NVFP4"}]`),
			),
			repo:   "org/Base-NVFP4",
			wantID: 2,
		},
		{
			name: "smallest ID wins when listed first",
			specs: weightRepoSpecs(
				weightRepoModel(2, `[{"repo_id":"org/Shared"}]`),
				weightRepoModel(9, `[{"repo_id":"org/Shared"}]`),
			),
			repo:   "org/shared",
			wantID: 2,
		},
		{
			name: "smallest ID wins when listed last",
			specs: weightRepoSpecs(
				weightRepoModel(9, `[{"repo_id":"org/Shared"}]`),
				weightRepoModel(6, `[{"repo_id":"org/Unrelated"}]`),
				weightRepoModel(2, `[{"repo_id":"org/Shared"}]`),
			),
			repo:   "org/shared",
			wantID: 2,
		},
		{name: "empty query", specs: weightRepoModel(1, `[{"repo_id":""}]`), repo: "  "},
		{name: "no models", repo: "org/Base"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ModelDeclaringWeightRepo(tc.specs, tc.repo)
			if tc.wantID == 0 {
				if ok || got != 0 {
					t.Fatalf("ModelDeclaringWeightRepo(%q) = %d, %v; want no match", tc.repo, got, ok)
				}
				return
			}
			if !ok || got != tc.wantID {
				t.Fatalf("ModelDeclaringWeightRepo(%q) = %d, %v; want model ID %d", tc.repo, got, ok, tc.wantID)
			}
		})
	}
}
