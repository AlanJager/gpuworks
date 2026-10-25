package seed

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/chipmatch"
)

func TestAscend910CatalogPredicatesAcceptProductSuffixes(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}

	tested := 0
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if !strings.EqualFold(spec.GPUs.Vendor, "ascend") || !strings.Contains(strings.ToUpper(spec.GPUs.Name), "910") {
				continue
			}
			tested++
			predicate := chipmatch.Predicate{Vendor: spec.GPUs.Vendor, Name: spec.GPUs.Name, CC: spec.GPUs.CC}
			where := model.ModelID + "/" + spec.Name
			supported := []string{"Ascend 910B1", "Ascend 910B2C", "Ascend 910C3"}
			unrelated := []string{"Ascend 310P", "Ascend 910A"}
			// The A3 GLM recipe uses a 910C-specific image and parallel layout;
			// product suffixes do not make the older 910B family compatible.
			if model.ModelID == "gpuworks/glm-5.3-flash-910c" {
				supported = []string{"Ascend 910C", "Ascend 910C3"}
				unrelated = append(unrelated, "Ascend 910B1", "Ascend 910B2C")
			}
			for _, name := range supported {
				if result := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate); !result.Matched {
					t.Errorf("%s rejects supported card %q with predicate %q", where, name, spec.GPUs.Name)
				}
			}
			for _, name := range unrelated {
				if result := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate); result.Matched {
					t.Errorf("%s accepts unrelated card %q with predicate %q", where, name, spec.GPUs.Name)
				}
			}
		}
	}
	if tested == 0 {
		t.Fatal("embedded catalog has no Ascend 910 predicates")
	}
}
