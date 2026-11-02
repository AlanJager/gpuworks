package modelmetadata

import (
	"context"
	"slices"
	"testing"
)

func TestCheckpointPreservesFormatOwnedFileSelection(t *testing.T) {
	for _, tc := range []struct {
		name, format string
		files        map[string]string
		want         []string
		incomplete   bool
		noEntry      bool
	}{
		{
			name: "native mistral ignores unrelated transformers index", format: "mistral",
			files: map[string]string{"params.json": `{"dim":4096}`, "tekken.json": `{}`, "consolidated.safetensors": "native weights", "model.safetensors.index.json": `{"weight_map":{"x":"missing-hf-export.safetensors"}}`},
			want:  []string{"consolidated.safetensors"},
		},
		{
			name: "native mistral does not claim unrelated weights", noEntry: true,
			files: map[string]string{"params.json": `{"dim":4096}`, "tekken.json": `{}`, "adapter_model.safetensors": "adapter weights"},
		},
		{
			name: "diffusers component ignores unrelated transformers index", format: "diffusers",
			files: map[string]string{"model_index.json": `{"_class_name":"ExamplePipeline","transformer":["diffusers","FluxTransformer2DModel"]}`, "transformer/config.json": `{}`, "transformer/diffusion_pytorch_model.bin": "diffusion weights", "transformer/model.safetensors.index.json": `{"weight_map":{"x":"missing-hf-export.safetensors"}}`},
			want:  []string{"transformer/diffusion_pytorch_model.bin"},
		},
		{
			name: "dense component ignores unrelated broken index", format: "sentence-transformers",
			files: map[string]string{"modules.json": `[{"path":"dense","type":"sentence_transformers.models.Dense"}]`, "dense/config.json": `{"in_features":4,"out_features":2}`, "dense/model.safetensors": "dense weights", "dense/optimizer.safetensors.index.json": `{`},
			want:  []string{"dense/model.safetensors"},
		},
		{
			name: "dense component does not adopt adapter weights", format: "sentence-transformers", incomplete: true,
			files: map[string]string{"modules.json": `[{"path":"dense","type":"sentence_transformers.models.Dense"}]`, "dense/config.json": `{"in_features":4,"out_features":2}`, "dense/adapter_model.safetensors": "adapter weights"},
		},
		{
			name: "canonical index selects default checkpoint alongside another export", format: "transformers",
			files: map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors.index.json": `{"weight_map":{"x":"default.safetensors"}}`, "alternate.safetensors.index.json": `{"weight_map":{"x":"alternate.safetensors"}}`, "default.safetensors": "default weights", "alternate.safetensors": "alternate weights"},
			want:  []string{"default.safetensors"},
		},
		{
			name: "indexed bin precedes incomplete unindexed safetensors export", format: "transformers",
			files: map[string]string{"config.json": `{"model_type":"example"}`, "pytorch_model.bin.index.json": `{"weight_map":{"x":"weights.bin"}}`, "weights.bin": "indexed weights", "model-00001-of-00002.safetensors": "unfinished alternative export"},
			want:  []string{"weights.bin"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{}, memory(tc.files))
			if err != nil {
				t.Fatal(err)
			}
			if tc.noEntry {
				if len(r.Entries) != 0 {
					t.Fatalf("unrelated weights adopted: %+v", r)
				}
				return
			}
			if len(r.Entries) != 1 {
				t.Fatalf("previously recognized layout rejected: %+v", r)
			}
			e := r.Entries[0]
			if e.Format != tc.format || e.Incomplete != tc.incomplete || (tc.want != nil && !slices.Equal(e.Files, tc.want)) {
				t.Fatalf("entry=%+v issues=%+v", e, r.Issues)
			}
		})
	}
}
