package seed

import (
	"regexp"
	"testing"
)

// Template env entries are injected into the container verbatim. Keys must be
// conventional environment variable names so a stray configuration field
// cannot ride along into every deployment.
func TestCatalogSpecEnvKeysAreEnvironmentVariableNames(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	name := regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	checked := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for key := range spec.Env {
				checked++
				if !name.MatchString(key) {
					t.Errorf("%s/%s env key %q is not an environment variable name", model.ModelID, spec.Name, key)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no catalog env entries were checked")
	}
}
