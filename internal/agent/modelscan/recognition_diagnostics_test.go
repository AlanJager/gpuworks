package modelscan

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRecognitionDiagnosticsKeepScanClassificationAndTraversal(t *testing.T) {
	root := t.TempDir()
	write(t, p(root, "config-only/config.json"), `{"model_type":"example"}`)
	write(t, p(root, "tokenizer-only/tokenizer.json"), `{}`)
	write(t, p(root, "docs-only/README.md"), "Model documentation")
	write(t, p(root, "weights-only/model.safetensors"), "weights")
	write(t, p(root, "parent/config.json"), `{}`)
	hfModel(t, root, "parent/child")
	write(t, p(root, "invalid/config.json"), `{`)
	r, err := Scan(context.Background(), root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) != 1 || r.Candidates[0].RelPath != "parent/child" {
		t.Fatalf("diagnostics stopped descendant discovery: %+v", r)
	}
	skipped := skippedMap(r)
	for dir, want := range map[string]string{"config-only": SkipUnrecognizedLayout, "tokenizer-only": SkipUnrecognizedLayout, "docs-only": SkipMetadataOnly, "weights-only": SkipUnrecognizedLayout, "invalid": proto.ScanSkipInvalidMetadata} {
		if skipped[dir] != want {
			t.Errorf("%s: got %q want %q", dir, skipped[dir], want)
		}
	}
}
