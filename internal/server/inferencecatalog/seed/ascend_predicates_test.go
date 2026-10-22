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
			for _, name := range []string{"Ascend 910B1", "Ascend 910B2C", "Ascend 910C3"} {
				if result := chipmatch.Evaluate(chipmatch.GPU{Vendor: "ascend", Name: name}, predicate); !result.Matched {
					t.Errorf("%s rejects supported card %q with predicate %q", where, name, spec.GPUs.Name)
				}
			}
			for _, name := range []string{"Ascend 310P", "Ascend 910A"} {
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
