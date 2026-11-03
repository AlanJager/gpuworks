package inferencecatalog

import (
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/deploy"
)

// TestPackageImportPassesAnUndecodableDictionaryValueThrough holds the
// import boundary to what a template is for: the author's recipe reaches the
// engine as written, whatever the platform's dictionary knows. A value the
// dictionary cannot read imports with a warning naming the flag, and both of
// its tokens stay in the residual argv verbatim; a readable value becomes the
// typed field as before.
func TestPackageImportPassesAnUndecodableDictionaryValueThrough(t *testing.T) {
	loaded, err := LoadPackage(packageFixtureWithMaxModelLen(t, "abc"))
	if err != nil {
		t.Fatalf("package with --max-model-len abc was refused: %v", err)
	}
	warnings := CatalogWarnings(loaded.Catalog)
	if len(warnings) != 1 || warnings[0].Code() != "CATALOG_ARG_PASSED_THROUGH" {
		t.Fatalf("warnings = %+v", warnings)
	}
	got := warnings[0].Params()
	if got["model_id"] != "org/model" || got["spec"] != "bf16-balanced" || got["flag"] != "--max-model-len" || got["value"] != "abc" || got["key"] != "max_model_len" {
		t.Fatalf("warning params = %v", got)
	}
	spec := loaded.Catalog.Models[0].Specs[0]
	params, residual, err := deploy.ReverseArgs(spec.Engine, spec.Args)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := params["max_model_len"]; set || strings.Join(residual, " ") != "--max-model-len abc" {
		t.Fatalf("params=%#v residual=%#v", params, residual)
	}

	loaded, err = LoadPackage(packageFixtureWithMaxModelLen(t, "4096"))
	if err != nil {
		t.Fatalf("package with --max-model-len 4096 was rejected: %v", err)
	}
	if warnings := CatalogWarnings(loaded.Catalog); len(warnings) != 0 {
		t.Fatalf("readable value warned: %+v", warnings)
	}
	spec = loaded.Catalog.Models[0].Specs[0]
	params, residual, err = deploy.ReverseArgs(spec.Engine, spec.Args)
	if err != nil {
		t.Fatal(err)
	}
	if params["max_model_len"] != int64(4096) || len(residual) != 0 {
		t.Fatalf("params=%#v residual=%#v", params, residual)
	}
}

// TestSpecValidationLeavesUnownedAndSharedFlagsAlone keeps the import check
// from growing into a general argv opinion: the dictionary judges only the
// flags it alone owns, and a catalog author's own vocabulary stays theirs.
func TestSpecValidationLeavesUnownedAndSharedFlagsAlone(t *testing.T) {
	for _, args := range [][]string{
		{"--max-model-len", "4096", "--served-model-name-suffix", "abc"},
		// --speculative-config carries four dictionary keys on vLLM, so no
		// single key's type applies to its value.
		{"--speculative-config", "{not json}"},
		{"--kv-transfer-config", "{not json}"},
		{"--trust-remote-code", "--enforce-eager"},
		{"--dtype", "half", "--kv-cache-dtype", "FP8_E4M3"},
	} {
		catalog := flatFixture()
		catalog.Models[0].Specs[0].Args = args
		if err := ValidateFlatCatalog(&catalog); err != nil {
			t.Errorf("args %v rejected: %v", args, err)
		}
	}
}

// packageFixtureWithMaxModelLen rewrites the fixture's context length in the
// package's own YAML rather than through MarshalModelYAML, which validates and
// would refuse to write the broken document this test needs.
func packageFixtureWithMaxModelLen(t *testing.T, value string) fstest.MapFS {
	t.Helper()
	const path = "models/org/model/model.yaml"
	files := packageFixture(t)
	text := string(files[path].Data)
	if strings.Count(text, `"32768"`) != 1 {
		t.Fatalf("fixture no longer writes exactly one --max-model-len 32768:\n%s", text)
	}
	rewritten := strings.Replace(text, `"32768"`, strconv.Quote(value), 1)
	files[path] = &fstest.MapFile{Data: []byte(rewritten)}
	return files
}
