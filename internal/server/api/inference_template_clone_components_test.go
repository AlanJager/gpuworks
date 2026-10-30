package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/packages"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The built-in GLM-5.3-Flash TensorFold template mounts its DFlash2 draft, a
// model of its own, and carries README and adapter assets, so a clone is
// written as a package. That package used to hold only the clone and failed
// validation ("references missing original model incoai/glm-5.3-flash-dflash2");
// it now carries the draft as a component-only entry. Editing the clone's YAML
// rebuilds the package the same way and leaves the draft's own row alone.
func TestCloneAndEditATemplateThatMountsAComponentModel(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "clone-components.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	packageStore, err := packages.New(filepath.Join(t.TempDir(), "packages"))
	if err != nil {
		t.Fatal(err)
	}
	// Startup order of the server: the embedded package, then its projection.
	archive, err := seed.Archive()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := packageStore.PutArchive(bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	if err := seedInferenceCatalogFixture(database); err != nil {
		t.Fatal(err)
	}
	draftBefore, err := database.InferenceModelByModelID("incoai/glm-5.3-flash-dflash2")
	if err != nil {
		t.Fatal(err)
	}
	handler := &apiHandlers{d: Deps{Store: database, CatalogPackages: packageStore}}

	recorder := httptest.NewRecorder()
	handler.cloneInferenceTemplate(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/clone", bytes.NewBufferString(`{"source_model_id":"mia-ailab/glm-5.3-flash"}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("clone status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	cloned, err := database.InferenceModelByModelID("mia-ailab/glm-5.3-flash-copy")
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Source != "user" || cloned.PackageDigest == "" || len(cloned.Specs) != 2 {
		t.Fatalf("cloned model=%+v", cloned)
	}
	for _, asset := range []string{"assets/tensorfold-adapter.tar.gz", "assets/tensorfold-tp-adapter.tar.gz"} {
		if data, err := packageStore.ReadAsset(cloned.PackageDigest, cloned.ModelID, asset); err != nil || len(data) == 0 {
			t.Fatalf("cloned asset %s: %d bytes, err=%v", asset, len(data), err)
		}
	}
	if readme, err := packageStore.ReadDocumentation(cloned.PackageDigest, cloned.ModelID, "README.zh-CN.md"); err != nil || len(readme) == 0 {
		t.Fatalf("cloned README: %d bytes, err=%v", len(readme), err)
	}
	var packed bytes.Buffer
	if err := packageStore.Pack(cloned.PackageDigest, &packed); err != nil {
		t.Fatal(err)
	}
	clonedPackage, err := inferencecatalog.ReadPackageArchive(bytes.NewReader(packed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	carried := map[string]int{}
	for _, model := range clonedPackage.Package.Catalog.Models {
		carried[model.ModelID] = len(model.Specs)
		if model.ModelID == "incoai/glm-5.3-flash-dflash2" && (model.ReadmeZHCN != "" || model.ReadmeEN != "") {
			t.Fatalf("the component entry names documentation the package does not hold: %+v", model)
		}
	}
	if len(carried) != 2 || carried["mia-ailab/glm-5.3-flash-copy"] != 2 || carried["incoai/glm-5.3-flash-dflash2"] != 0 {
		t.Fatalf("cloned package models (spec counts) = %v, want the clone and the component-only draft", carried)
	}

	editable, err := inferencecatalog.FromFlatStoreModel(*cloned)
	if err != nil {
		t.Fatal(err)
	}
	editable.DisplayTitle = "GLM-5.3-Flash (my recipe)"
	// The template editor saves a document holding only the template it edits
	// (the shape of inferencecatalog.EditableYAML); the draft it mounts is not in it.
	editedYAML, err := yaml.Marshal(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{editable}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inferencecatalog.ParseYAML(editedYAML); err == nil {
		t.Fatal("the edited document validates on its own; it no longer exercises the server's component models")
	}
	recorder = httptest.NewRecorder()
	handler.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", bytes.NewReader(editedYAML)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("edit status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	edited, err := database.InferenceModelByModelID(cloned.ModelID)
	if err != nil || edited.DisplayTitle != "GLM-5.3-Flash (my recipe)" || edited.PackageDigest == cloned.PackageDigest || len(edited.Specs) != 2 {
		t.Fatalf("edited model=%+v err=%v", edited, err)
	}
	draftAfter, err := database.InferenceModelByModelID("incoai/glm-5.3-flash-dflash2")
	if err != nil {
		t.Fatal(err)
	}
	if draftAfter.Source != draftBefore.Source || draftAfter.PackageDigest != draftBefore.PackageDigest || draftAfter.DisplayTitle != draftBefore.DisplayTitle {
		t.Fatalf("editing the clone rewrote the draft's row: before=%+v after=%+v", draftBefore, draftAfter)
	}

	// A component the server does not have is still refused.
	editable.Specs[0].Components[1].ModelID = "nobody/missing-draft"
	missingYAML, err := yaml.Marshal(inferencecatalog.FlatCatalog{Models: []inferencecatalog.InferenceModel{editable}})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	handler.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import?overwrite=true", bytes.NewReader(missingYAML)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "references missing original model nobody/missing-draft") {
		t.Fatalf("missing component status/body=%d %s", recorder.Code, recorder.Body.String())
	}
}
