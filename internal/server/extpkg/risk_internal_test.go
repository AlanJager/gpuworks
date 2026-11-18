package extpkg

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The risk table classifies exactly the schema's permissions: a new
// permission gets a class in the same change, and a class names no
// permission the schema lacks.
func TestRiskTableMatchesThePermissionVocabulary(t *testing.T) {
	vocabulary := extapi.Permissions()
	for _, p := range vocabulary {
		if _, ok := permissionRisks[p]; !ok {
			t.Errorf("%s has no risk class", p)
		}
	}
	for p := range permissionRisks {
		if !slices.Contains(vocabulary, p) {
			t.Errorf("%s is classified but not a permission of the schema", p)
		}
	}
}
