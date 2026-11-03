package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImportedArchIDSelectorReachesPlannerGPUValidation(t *testing.T) {
	const catalogYAML = `models:
  - model_id: org/arch-model
    source: user
    display_title: Architecture model
    specs:
      - name: gfx1151-bf16
        display_profile: general
        scenario: balanced
        engine: vllm
        image: example/engine:tag
        weights:
          quantization: bf16
          sources:
            - huggingface: org/arch-model
        gpus: {vendor: amd, name: "Radeon 8060S", arch_id: gfx1151, display_target: amd395}
    unsupported:
      - gpus: {vendor: amd, name: "Radeon 8060S", arch_id: gfx1150, display_target: amd395}
        reason: gfx1150 is unsupported
`
	catalog, err := inferencecatalog.ParseYAML([]byte(catalogYAML))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := inferencecatalog.ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open("file:planning_arch_id_import_roundtrip?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ImportUserInferenceCatalog(rows, true); err != nil {
		t.Fatal(err)
	}
	model, err := st.InferenceModelByModelID("org/arch-model")
	if err != nil {
		t.Fatal(err)
	}
	persistedPredicate, err := chipmatch.DecodePredicate(model.Specs[0].GPUs)
	if err != nil || persistedPredicate.ArchID != "gfx1151" {
		t.Fatalf("persisted GPU selector=%+v err=%v", persistedPredicate, err)
	}
	persistedUnsupported, err := chipmatch.DecodeUnsupported(model.Unsupported)
	if err != nil || len(persistedUnsupported) != 1 || persistedUnsupported[0].GPUs.ArchID != "gfx1150" {
		t.Fatalf("persisted unsupported rules=%+v err=%v", persistedUnsupported, err)
	}
	layer := deploy.CatalogSelection{
		SpecName: model.Specs[0].Name, Quantization: model.Specs[0].Quantization,
		GPUs: []byte(model.Specs[0].GPUs), Unsupported: []byte(model.Unsupported),
	}
	validate, err := TemplateGPUValidator(layer)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(store.GPU{Name: "Radeon 8060S", Vendor: "amd", ArchID: "gfx1151"}); err != nil {
		t.Fatalf("matching persisted architecture rejected by planner validator: %v", err)
	}
	if err := validate(store.GPU{Name: "Radeon 8060S", Vendor: "amd", ArchID: "gfx1150"}); err == nil {
		t.Fatal("planner accepted same-name GPU with a different architecture")
	}
	if err := validate(store.GPU{Name: "Radeon 8060S", Vendor: "amd"}); err == nil {
		t.Fatal("planner accepted missing architecture fact")
	}
}
