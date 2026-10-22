package seed

import (
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestSGLangCatalogDeclaresMetricsOnlyForNonDiffusionEntries(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			count := 0
			for _, arg := range spec.Args {
				if arg == "--enable-metrics" {
					count++
				}
			}
			if count > 1 {
				t.Errorf("%s/%s declares metrics %d times", model.ModelID, spec.Name, count)
			}
			enabled := slices.Contains(spec.Args, "--enable-metrics")
			if spec.Engine != engines.SGLang {
				if enabled {
					t.Errorf("%s/%s passes SGLang's metrics flag to %s", model.ModelID, spec.Name, spec.Engine)
				}
				continue
			}
			diffusion := false
			if spec.API != nil {
				for _, op := range spec.API.Operations {
					diffusion = diffusion || op.Type == inferenceapi.OperationVideoGenerate || op.Type == inferenceapi.OperationImageGenerate
				}
			}
			if enabled == diffusion {
				t.Errorf("%s/%s metrics=%v, diffusion=%v", model.ModelID, spec.Name, enabled, diffusion)
			}
		}
	}
}
