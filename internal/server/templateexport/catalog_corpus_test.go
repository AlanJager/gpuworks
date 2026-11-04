package templateexport

import (
	"errors"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// Every built-in spec a single-node deployment can be made from exports: the
// deployment is materialized the way the create path does it (dictionary
// defaults included), and saving it as a template must give a document that
// imports and reproduces the engine command. This is the corpus the arg
// export is checked against; a spec that fails here is a launch the platform
// runs but cannot write down.
func TestEveryBuiltinSpecExports(t *testing.T) {
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	exported, skipped := 0, 0
	var failures []string
	for mi := range catalog.Models {
		model := catalog.Models[mi]
		for si := range model.Specs {
			spec := model.Specs[si]
			if len(spec.Assets) > 0 || len(spec.Components) > 0 || spec.PDMode != "" {
				skipped++
				continue
			}
			materialized, err := deploy.Materialize(deploy.MaterializeInput{Engine: spec.Engine, TemplateArgs: spec.Args})
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s materialize: %v", model.ModelID, spec.Name, err))
				continue
			}
			in := Input{
				DeploymentID: 1, DeploymentName: "corpus", Engine: spec.Engine, Image: spec.Image,
				Quick: materialized.Quick, TemplateArgs: materialized.TemplateResidual, TemplateEnv: spec.Env,
				Template: &model, TemplateSpec: &spec,
				GPUs: []GPUFact{{Name: "Test Card", Vendor: spec.GPUs.Vendor, MemoryMiB: 81920}}, GPUCount: 1,
			}
			out, err := Build(in)
			if err != nil {
				var product *messages.ProductError
				if errors.As(err, &product) {
					err = fmt.Errorf("%s %v", product.Code(), product.Params())
				}
				failures = append(failures, fmt.Sprintf("%s/%s: %v", model.ModelID, spec.Name, err))
				continue
			}
			if _, err := inferencecatalog.ParseYAML([]byte(out.YAML)); err != nil {
				failures = append(failures, fmt.Sprintf("%s/%s import: %v", model.ModelID, spec.Name, err))
				continue
			}
			exported++
		}
	}
	for _, failure := range failures {
		t.Error(failure)
	}
	t.Logf("exported %d built-in specs, skipped %d with assets, components or a PD mode", exported, skipped)
	if exported < 500 {
		t.Fatalf("exported only %d specs: the corpus no longer covers the catalog", exported)
	}
}
