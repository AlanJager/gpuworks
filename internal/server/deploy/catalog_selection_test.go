package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A row the writer stored re-encodes byte for byte: the snapshot column is
// history, and a reader that rewrites it must not reformat the parts it only
// read.
func TestCatalogSelectionRoundTripsAStoredDocument(t *testing.T) {
	stored := `{"acquisition":{"model_source":{"kind":"public"}},"api":{"operations":[]},"components":[{"mount_path":"/vision","name":"vision","role":"encoder","store_model_id":9}],"engine":"vllm","extra_args":["--dtype","bfloat16"],"gpus":{"min_count":1},"image":"registry.example/org/spec:1","launch_contract":"explicit","matched_by":"spec","model_id":"org/fixture-8b","prestart_script":"set -a\nexport X=1","runtime_weight_bytes":7516192768,"spec_name":"balanced","spec_source":"upstream","template_id":7,"weight_bytes":8589934592,"weight_provider":"huggingface","weight_repo_id":"org/fixture-8b","future_field":{"nested":[1,2]}}`
	selection, err := DecodeCatalogSelection([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if _, known := catalogSelectionKeys["prestart_script"]; known {
		t.Fatal("catalogSelectionKeys knows a key the type does not declare")
	}
	if selection.SpecName != "balanced" || selection.TemplateID != 7 {
		t.Fatalf("decoded selection = %+v", selection)
	}
	// launch_contract is history: rows written before ADR 0031 carry it, and
	// it is kept verbatim like any other key this version does not read.
	for _, key := range []string{"future_field", "launch_contract"} {
		if _, ok := selection.Extra[key]; !ok {
			t.Fatalf("unknown field %s was dropped: %+v", key, selection.Extra)
		}
	}
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The stored form has sorted top-level keys; the codec must reproduce it
	// (including the unknown field and the nested order it was written with).
	if normalizeSortedJSON(t, stored) != normalizeSortedJSON(t, string(encoded)) {
		t.Fatalf("round trip changed the document:\n got %s\nwant %s", encoded, stored)
	}
	if string(encoded) != `{"acquisition":{"model_source":{"kind":"public"}},"api":{"operations":[]},"components":[{"mount_path":"/vision","name":"vision","role":"encoder","store_model_id":9}],"engine":"vllm","extra_args":["--dtype","bfloat16"],"future_field":{"nested":[1,2]},"gpus":{"min_count":1},"image":"registry.example/org/spec:1","launch_contract":"explicit","matched_by":"spec","model_id":"org/fixture-8b","prestart_script":"set -a\nexport X=1","runtime_weight_bytes":7516192768,"spec_name":"balanced","spec_source":"upstream","template_id":7,"weight_bytes":8589934592,"weight_provider":"huggingface","weight_repo_id":"org/fixture-8b"}` {
		t.Fatalf("round trip is not byte-stable: %s", encoded)
	}
}

// A document that is not an object is a row nobody can read; an empty document
// is a deployment with no catalog template, which is a state.
func TestDecodeCatalogSelectionRejectsNonObjects(t *testing.T) {
	for _, document := range []string{`[]`, `"text"`, `null`, `{`} {
		if _, err := DecodeCatalogSelection([]byte(document)); err == nil {
			t.Errorf("DecodeCatalogSelection(%s) accepted a non-object", document)
		}
	}
	selection, err := DecodeCatalogSelection(nil)
	if err != nil || selection.ModelID != "" || len(selection.Extra) != 0 {
		t.Fatalf("DecodeCatalogSelection(empty) = %+v, %v", selection, err)
	}
}

// The setter is how a stored row is amended: it replaces its own key and
// leaves every other byte of the document alone.
func TestCatalogSelectionSettersKeepTheRestOfTheDocument(t *testing.T) {
	stored := `{"extra_args":["--dtype","bfloat16"],"launch_contract":"explicit","matched_by":"spec","spec_name":"balanced"}`
	selection, err := DecodeCatalogSelection([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.SetTemplateArgs([]string{"--max-model-len", "4096"}); err != nil {
		t.Fatal(err)
	}
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"extra_args":["--max-model-len","4096"],"launch_contract":"explicit","matched_by":"spec","spec_name":"balanced"}`
	if string(encoded) != want {
		t.Fatalf("encode = %s, want %s", encoded, want)
	}
	if !reflect.DeepEqual(selection.ExtraArgs, []string{"--max-model-len", "4096"}) {
		t.Fatalf("typed view = %+v", selection)
	}
}

// A selection built in this process is rendered the way the writer stored one:
// sorted top-level keys, the acquisition record verbatim, and unknown fields
// merged in for readers this version does not know about.
func TestCatalogSelectionEncodesAFreshSelection(t *testing.T) {
	selection := CatalogSelection{
		Acquisition: json.RawMessage(`{"image_pull":{"kind":"peer","source_node":"node-a"}}`),
		TemplateID:  7, ModelID: "org/fixture-8b", SpecSource: "upstream", SpecName: "balanced",
		Engine: "vllm", Image: "registry.example/org/spec:1",
		WeightProvider: "huggingface", WeightRepoID: "org/fixture-8b",
		GPUs: json.RawMessage(`{"min_count":1}`), ExtraArgs: []string{"--dtype", "bfloat16"},
		Placement: json.RawMessage(`{"language":"starlark","resolver":"def resolve(gpus):\n  return gpus"}`),
		MatchedBy: "spec",
	}
	selection.Extra = map[string]json.RawMessage{"future_field": json.RawMessage(`{"nested":[1,2]}`)}
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"acquisition":{"image_pull":{"kind":"peer","source_node":"node-a"}},"engine":"vllm","extra_args":["--dtype","bfloat16"],"future_field":{"nested":[1,2]},"gpus":{"min_count":1},"image":"registry.example/org/spec:1","matched_by":"spec","model_id":"org/fixture-8b","placement":{"language":"starlark","resolver":"def resolve(gpus):\n  return gpus"},"spec_name":"balanced","spec_source":"upstream","template_id":7,"weight_provider":"huggingface","weight_repo_id":"org/fixture-8b"}`
	if string(encoded) != want {
		t.Fatalf("encode = %s\n want %s", encoded, want)
	}
}

// A deployment created without a catalog template has no selection: the stored
// document is empty (or just the acquisition record, when the request stated
// one), not a row of zero-valued fields.
func TestCatalogSelectionEncodesAnEmptySelectionAsNothing(t *testing.T) {
	var selection CatalogSelection
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{}` {
		t.Fatalf("empty selection = %s", encoded)
	}

	// The same deployment can still carry an acquisition record.
	withAcquisition := CatalogSelection{Acquisition: json.RawMessage(`{"model_source":{"kind":"public"}}`)}
	encoded, err = withAcquisition.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"acquisition":{"model_source":{"kind":"public"}}}` {
		t.Fatalf("empty selection with acquisition = %s", encoded)
	}
}

// The writer this type replaces had two stored layouts, because it only
// round-tripped the document through a map[string]any when it had an
// acquisition record to insert: with one, nested objects came out key-sorted;
// without one, they kept the order the struct marshal produced. A fresh
// selection reproduces whichever applies to it, so neither class of request
// changes the bytes it stores.
func TestCatalogSelectionKeepsTheNestedLayoutTheWriterUsed(t *testing.T) {
	selection := CatalogSelection{
		TemplateID: 7, ModelID: "org/fixture-8b", SpecSource: "upstream", SpecName: "balanced",
		Engine: "vllm", Image: "registry.example/org/spec:1",
		WeightProvider: "huggingface", WeightRepoID: "org/fixture-8b",
		GPUs:      json.RawMessage(`{"min_count":1}`),
		MatchedBy: "spec",
		Components: []ComponentBinding{{
			Name: "vision", Role: "encoder", Provider: "huggingface", RepoID: "org/vision",
			MountPath: "/vision", StoreModelID: 9, WeightBytes: 1073741824,
		}},
	}
	withoutAcquisition, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// ComponentBinding marshals in declaration order: name, role, provider, ...
	if !strings.Contains(string(withoutAcquisition), `"components":[{"name":"vision","role":"encoder","provider":"huggingface"`) {
		t.Fatalf("nested bytes are not the struct's own layout: %s", withoutAcquisition)
	}

	withAcquisition := selection
	withAcquisition.Acquisition = json.RawMessage(`{"image_pull":{"kind":"peer","source_node":"node-a"}}`)
	encoded, err := withAcquisition.Encode()
	if err != nil {
		t.Fatal(err)
	}
	// The map round trip sorts the nested object's keys.
	if !strings.Contains(string(encoded), `"components":[{"mount_path":"/vision","name":"vision","provider":"huggingface"`) {
		t.Fatalf("acquisition-carrying document did not keep the mapped layout: %s", encoded)
	}
}

// A decoded selection is edited through its setters; the stored document is
// what a row means, so assigning a typed field directly changes the view and
// not the bytes. That is worth pinning: the alternative is a write that looks
// applied and is silently dropped on encode.
func TestCatalogSelectionIgnoresDirectFieldWrites(t *testing.T) {
	selection, err := DecodeCatalogSelection([]byte(`{"engine":"vllm","matched_by":"spec","spec_name":"balanced"}`))
	if err != nil {
		t.Fatal(err)
	}
	selection.Engine = "sglang"
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"engine":"vllm","matched_by":"spec","spec_name":"balanced"}` {
		t.Fatalf("a direct field write reached the document: %s", encoded)
	}
}

// A copy of a decoded selection shares the document it was decoded from, so a
// setter on the copy must not rewrite the original's bytes.
func TestCatalogSelectionSetterOnACopyLeavesTheOriginalAlone(t *testing.T) {
	original, err := DecodeCatalogSelection([]byte(`{"extra_args":["--dtype","bfloat16"],"launch_contract":"explicit","spec_name":"balanced"}`))
	if err != nil {
		t.Fatal(err)
	}
	copied := original
	if err := copied.SetTemplateArgs([]string{"--max-model-len", "4096"}); err != nil {
		t.Fatal(err)
	}
	copiedJSON, err := copied.Encode()
	if err != nil {
		t.Fatal(err)
	}
	originalJSON, err := original.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(copiedJSON) != `{"extra_args":["--max-model-len","4096"],"launch_contract":"explicit","spec_name":"balanced"}` {
		t.Fatalf("copy = %s", copiedJSON)
	}
	if string(originalJSON) != `{"extra_args":["--dtype","bfloat16"],"launch_contract":"explicit","spec_name":"balanced"}` {
		t.Fatalf("the copy's setter rewrote the original: %s", originalJSON)
	}
}

// The placement resolver and the acquisition record are the two sub-documents
// readers ask for by name; both answer from the stored bytes.
func TestCatalogSelectionSubDocumentAccessors(t *testing.T) {
	selection, err := DecodeCatalogSelection([]byte(`{"acquisition":{"image_pull":{"kind":"peer","source_node":"node-a"}},"placement":{"language":"starlark","resolver":"def resolve(gpus):\n  return gpus"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := selection.PlacementResolver(); got != "def resolve(gpus):\n  return gpus" {
		t.Fatalf("resolver = %q", got)
	}
	if got := string(selection.AcquisitionField("image_pull")); got != `{"kind":"peer","source_node":"node-a"}` {
		t.Fatalf("image_pull = %s", got)
	}
	if got := selection.AcquisitionField("missing"); got != nil {
		t.Fatalf("missing acquisition field = %s", got)
	}
	if got := (CatalogSelection{}).PlacementResolver(); got != "" {
		t.Fatalf("empty selection resolver = %q", got)
	}
}

// normalizeSortedJSON renders a document with its keys in a stable order so a
// test can compare documents whose top-level order differs.
func normalizeSortedJSON(t *testing.T, document string) string {
	t.Helper()
	value := map[string]any{}
	if err := json.Unmarshal([]byte(document), &value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// A snapshot written before templates could declare image sources has no
// image_sources key. It decodes to no sources, re-encodes without inventing the
// key, and offers none for its image.
func TestCatalogSelectionWithoutImageSourcesIsAnOldSnapshot(t *testing.T) {
	stored := `{"engine":"vllm","image":"ghcr.io/org/runner@sha256:aaaa","matched_by":"spec","spec_name":"balanced","template_id":7}`
	selection, err := DecodeCatalogSelection([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if selection.ImageSources != nil || selection.ImageSourcesFor("ghcr.io/org/runner@sha256:aaaa") != nil {
		t.Fatalf("an old snapshot declares no image sources: %+v", selection.ImageSources)
	}
	if _, extra := selection.Extra["image_sources"]; extra {
		t.Fatal("image_sources is a known key and must not land in Extra")
	}
	encoded, err := selection.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != stored {
		t.Fatalf("re-encoded old snapshot = %s, want %s", encoded, stored)
	}
}

// image_sources is a key of the document: it decodes into the typed view and
// the stored bytes are reproduced untouched, and a fresh selection writes it in
// sorted position, omitting it when there is none.
func TestCatalogSelectionCarriesImageSources(t *testing.T) {
	stored := `{"engine":"vllm","image":"ghcr.io/org/runner@sha256:aaaa","image_sources":["swr.example.com/org/runner@sha256:aaaa","ghcr.nju.edu.cn/org/runner@sha256:aaaa"],"matched_by":"spec","spec_name":"balanced","template_id":7}`
	selection, err := DecodeCatalogSelection([]byte(stored))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"swr.example.com/org/runner@sha256:aaaa", "ghcr.nju.edu.cn/org/runner@sha256:aaaa"}; !reflect.DeepEqual(selection.ImageSources, want) {
		t.Fatalf("image sources = %v, want %v", selection.ImageSources, want)
	}
	if _, extra := selection.Extra["image_sources"]; extra {
		t.Fatal("image_sources must be a known key, not an unknown field kept verbatim")
	}
	encoded, err := selection.Encode()
	if err != nil || string(encoded) != stored {
		t.Fatalf("round trip = %s, %v", encoded, err)
	}

	fresh := CatalogSelection{
		TemplateID: 7, SpecName: "balanced", Engine: "vllm", MatchedBy: "spec",
		Image: "ghcr.io/org/runner@sha256:aaaa", ImageSources: selection.ImageSources,
	}
	freshEncoded, err := fresh.Encode()
	if err != nil {
		t.Fatal(err)
	}
	wantFresh := `{"engine":"vllm","gpus":null,"image":"ghcr.io/org/runner@sha256:aaaa","image_sources":["swr.example.com/org/runner@sha256:aaaa","ghcr.nju.edu.cn/org/runner@sha256:aaaa"],"matched_by":"spec","model_id":"","spec_name":"balanced","spec_source":"","template_id":7,"weight_provider":"","weight_repo_id":""}`
	if string(freshEncoded) != wantFresh {
		t.Fatalf("fresh selection = %s\n want %s", freshEncoded, wantFresh)
	}
	fresh.ImageSources = nil
	bare, err := fresh.Encode()
	if err != nil || strings.Contains(string(bare), "image_sources") {
		t.Fatalf("a selection without sources must not write the key: %s, %v", bare, err)
	}
}

// The sources belong to the image the template names. Another image, however
// spelled in case or padded with space, gets none of them.
func TestCatalogSelectionImageSourcesForTheTemplateImageOnly(t *testing.T) {
	selection := CatalogSelection{Image: "ghcr.io/org/runner@sha256:aaaa", ImageSources: []string{"swr.example.com/org/runner:v1"}}
	for image, want := range map[string][]string{
		"ghcr.io/org/runner@sha256:aaaa":    {"swr.example.com/org/runner:v1"},
		"  GHCR.io/org/runner@sha256:aaaa ": {"swr.example.com/org/runner:v1"},
		"ghcr.io/org/runner@sha256:bbbb":    nil,
		"vllm/vllm-openai:latest":           nil,
		"":                                  nil,
	} {
		if got := selection.ImageSourcesFor(image); !reflect.DeepEqual(got, want) {
			t.Errorf("ImageSourcesFor(%q) = %v, want %v", image, got, want)
		}
	}
	if got := (CatalogSelection{Image: "x"}).ImageSourcesFor("x"); got != nil {
		t.Fatalf("a selection with no sources offered %v", got)
	}
}
