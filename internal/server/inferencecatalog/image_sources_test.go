package inferencecatalog

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

const (
	pinnedUpstream = "ghcr.io/org/model-runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	swrCopy        = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/model-runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	njuMirror      = "ghcr.nju.edu.cn/org/model-runner@sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
)

func imageSourcesDocument(sources string) []byte {
	return []byte(`models:
- model_id: Org/Model
  specs:
  - name: bf16-vllm-nvidia
    engine: vllm
    image: ` + pinnedUpstream + `
` + sources + `    weights: {quantization: bf16, sources: [{huggingface: Org/Model}]}
    gpus: {vendor: nvidia}
`)
}

// A template lists further sources for its runtime image the way it lists
// weight sources: in the order it prefers them, each a plain reference. They
// survive the YAML parse, the store row and the way back.
func TestImageSourcesParseStoreAndExportRoundTrip(t *testing.T) {
	catalog, err := ParseYAML(imageSourcesDocument("    image_sources:\n    - " + swrCopy + "\n    - " + njuMirror + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	spec := catalog.Models[0].Specs[0]
	if want := []string{swrCopy, njuMirror}; !reflect.DeepEqual(spec.ImageSources, want) {
		t.Fatalf("image_sources = %v, want %v", spec.ImageSources, want)
	}
	if spec.Image != pinnedUpstream {
		t.Fatalf("image = %q: the sources do not replace the template's identity", spec.Image)
	}

	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if want := `["` + swrCopy + `","` + njuMirror + `"]`; rows[0].Specs[0].ImageSources != want {
		t.Fatalf("image_sources column = %s, want %s", rows[0].Specs[0].ImageSources, want)
	}
	restored, err := FromFlatStoreModel(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Specs[0].ImageSources, spec.ImageSources) {
		t.Fatalf("restored image_sources = %v", restored.Specs[0].ImageSources)
	}

	exported, err := MarshalYAML(FlatCatalog{Models: []InferenceModel{restored}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exported), "image_sources:") {
		t.Fatalf("export lost image_sources:\n%s", exported)
	}
	reparsed, err := ParseYAML(exported)
	if err != nil {
		t.Fatalf("export does not import again: %v", err)
	}
	if !reflect.DeepEqual(reparsed.Models[0].Specs[0].ImageSources, spec.ImageSources) {
		t.Fatalf("reimported image_sources = %v", reparsed.Models[0].Specs[0].ImageSources)
	}
}

// A spec that declares none stores and exports exactly what it did before the
// field existed.
func TestSpecWithoutImageSourcesStoresAndExportsNothing(t *testing.T) {
	catalog, err := ParseYAML(imageSourcesDocument(""))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ToFlatStoreModels(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Specs[0].ImageSources != "" {
		t.Fatalf("image_sources column = %q, want blank", rows[0].Specs[0].ImageSources)
	}
	exported, err := MarshalYAML(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), "image_sources") {
		t.Fatalf("export mentions image_sources:\n%s", exported)
	}
}

func TestImageSourcesAreNormalizedAndChecked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []string
		want    []string
		wantErr string
		image   string
	}{
		{name: "trimmed and kept in order", sources: []string{"  " + njuMirror + " ", swrCopy}, want: []string{njuMirror, swrCopy}},
		{name: "tag references are accepted for a digest-pinned image", sources: []string{"swr.example.com/org/model-runner:v0.6.0"}, want: []string{"swr.example.com/org/model-runner:v0.6.0"}},
		{name: "another digest is accepted", sources: []string{"ghcr.io/org/model-runner@sha256:" + strings.Repeat("a", 64)}, want: []string{"ghcr.io/org/model-runner@sha256:" + strings.Repeat("a", 64)}},
		{name: "the image itself is dropped", sources: []string{pinnedUpstream, swrCopy}, want: []string{swrCopy}},
		{name: "only the image itself leaves none", sources: []string{" " + pinnedUpstream + " "}, want: nil},
		{name: "a tag differing only in case is another reference", sources: []string{"swr.example.com/org/x:V1", "swr.example.com/org/x:v1"}, want: []string{"swr.example.com/org/x:V1", "swr.example.com/org/x:v1"}},
		{name: "empty list", sources: []string{}, want: nil},
		{name: "empty entry", sources: []string{swrCopy, "  "}, wantErr: "image_sources[1] is empty"},
		{name: "whitespace inside", sources: []string{"swr.example.com/org/model runner:v1"}, wantErr: "contains whitespace"},
		{name: "tab inside", sources: []string{"swr.example.com/org/model\trunner:v1"}, wantErr: "contains whitespace"},
		{name: "url", sources: []string{"https://swr.example.com/org/model-runner.tar"}, wantErr: "not a URL"},
		{name: "scheme only inside", sources: []string{"docker://swr.example.com/org/model-runner:v1"}, wantErr: "not a URL"},
		{name: "too long", sources: []string{"swr.example.com/" + strings.Repeat("a", 512)}, wantErr: "exceeds 512 characters"},
		{name: "duplicate", sources: []string{swrCopy, njuMirror, swrCopy}, wantErr: "image_sources[2]"},
		{name: "duplicate as the registry resolves it", sources: []string{"org/x:v1", "docker.io/org/x:v1"}, wantErr: "is listed twice"},
		{name: "duplicate host spelled in another case", sources: []string{"Swr.Example.com/org/x:v1", "swr.example.com/org/x:v1"}, wantErr: "is listed twice"},
		{name: "the image in its docker hub spelling is dropped", sources: []string{"docker.io/library/nginx:1"}, want: nil, image: "nginx:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := flatFixture()
			catalog.Models[0].Specs[0].Image = pinnedUpstream
			if tc.image != "" {
				catalog.Models[0].Specs[0].Image = tc.image
			}
			catalog.Models[0].Specs[0].ImageSources = tc.sources
			err := ValidateFlatCatalog(&catalog)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				if !strings.Contains(err.Error(), "model org/model spec bf16-balanced") {
					t.Fatalf("error %q does not name the model and spec", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := catalog.Models[0].Specs[0].ImageSources; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("image_sources = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// image stays required: sources are further places to get it, not a substitute.
func TestImageSourcesDoNotReplaceTheRequiredImage(t *testing.T) {
	catalog := flatFixture()
	catalog.Models[0].Specs[0].Image = ""
	catalog.Models[0].Specs[0].ImageSources = []string{swrCopy}
	if err := ValidateFlatCatalog(&catalog); err == nil || !strings.Contains(err.Error(), "requires name, engine, and image") {
		t.Fatalf("error = %v", err)
	}
}

// Unknown keys are still refused: a misspelled field must not become a silently
// ignored one.
func TestImageSourcesTypoIsRejectedAsUnknownKey(t *testing.T) {
	if _, err := ParseYAML(imageSourcesDocument("    image_source:\n    - " + swrCopy + "\n")); err == nil || !strings.Contains(err.Error(), "image_source") {
		t.Fatalf("error = %v, want the unknown key named", err)
	}
	if _, err := ParseYAML(imageSourcesDocument("    image_sources: " + swrCopy + "\n")); err == nil {
		t.Fatal("a scalar image_sources must be refused")
	}
}

func TestDecodeImageSources(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
		wantErr   bool
	}{
		{name: "blank", raw: ""},
		{name: "whitespace", raw: " \n"},
		{name: "null", raw: "null"},
		{name: "empty", raw: "[]"},
		{name: "valid", raw: `["a/b:1","c/d:2"]`, want: []string{"a/b:1", "c/d:2"}},
		{name: "undecodable", raw: "not json", wantErr: true},
		{name: "wrong shape", raw: `{"a":1}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeImageSources(tc.raw)
			if (err != nil) != tc.wantErr || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("DecodeImageSources(%q) = %#v, %v", tc.raw, got, err)
			}
		})
	}
}

// A stored row that does not decode is named when it is turned back into a
// portable model.
func TestFromFlatStoreModelRefusesUndecodableImageSources(t *testing.T) {
	rows, err := ToFlatStoreModels(flatFixture())
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Specs[0].ImageSources = "not json"
	_, err = FromFlatStoreModel(rows[0])
	if err == nil || !strings.Contains(err.Error(), "org/model/bf16-balanced image_sources") {
		t.Fatalf("error = %v", err)
	}
}

// The editor schema and the import check state one rule: an entry the schema
// accepts on its own is one the import accepts, and the reverse. Duplicates and
// the entry equal to image are list-level rules the import adds (described in
// the schema's text), so single entries are what is compared here.
func TestEditorSchemaAgreesWithTheImportCheck(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(mustJSONSchema(t), &schema); err != nil {
		t.Fatal(err)
	}
	specs := schema["properties"].(map[string]any)["models"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["specs"].(map[string]any)
	property := specs["items"].(map[string]any)["properties"].(map[string]any)["image_sources"].(map[string]any)
	if property["uniqueItems"] != true {
		t.Fatalf("image_sources schema = %v, want uniqueItems", property)
	}
	items := property["items"].(map[string]any)
	pattern := regexp.MustCompile(items["pattern"].(string))
	forbidden := regexp.MustCompile(items["not"].(map[string]any)["pattern"].(string))
	minLength, maxLength := int(items["minLength"].(float64)), int(items["maxLength"].(float64))
	if maxLength != maxImageRefLength {
		t.Fatalf("schema maxLength = %d, want %d", maxLength, maxImageRefLength)
	}
	schemaAccepts := func(value string) bool {
		length := utf8.RuneCountInString(value)
		return length >= minLength && length <= maxLength && pattern.MatchString(value) && !forbidden.MatchString(value)
	}
	for _, value := range []string{
		swrCopy, "  " + njuMirror + " ", "nginx:1", "swr.example.com/org/x:V1",
		"", "   ", "swr.example.com/org/model runner:v1", "swr.example.com/org/model\trunner:v1",
		"https://swr.example.com/x.tar", "docker://swr.example.com/x:v1",
		"swr.example.com/" + strings.Repeat("a", maxImageRefLength-len("swr.example.com/")),
		"swr.example.com/" + strings.Repeat("a", maxImageRefLength),
		strings.Repeat("é", maxImageRefLength),
	} {
		_, err := normalizeImageSources("org/model", "spec", pinnedUpstream, []string{value})
		if importAccepts := err == nil; importAccepts != schemaAccepts(value) {
			t.Errorf("%q: import accepts = %v (err %v), schema accepts = %v", value, importAccepts, err, schemaAccepts(value))
		}
	}
}
