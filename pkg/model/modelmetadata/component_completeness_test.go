package modelmetadata

import (
	"context"
	"strings"
	"testing"
)

func TestCompositeMissingWeights(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
	}{
		{"diffusers", map[string]string{"model_index.json": `{"_class_name":"FluxPipeline","transformer":["diffusers","FluxTransformer2DModel"]}`, "transformer/config.json": `{"_class_name":"FluxTransformer2DModel"}`}},
		{"sentence_dense", map[string]string{"modules.json": `[{"path":"","type":"sentence_transformers.models.Transformer"},{"path":"1_Dense","type":"sentence_transformers.models.Dense"}]`, "config.json": `{"model_type":"bert"}`, "model.safetensors": "fixture-existing-transformer-weights", "1_Dense/config.json": `{"in_features":4,"out_features":2}`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{Path: "."}, memory(c.files))
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Entries) != 1 {
				t.Fatalf("not recognized: %+v", r)
			}
			if !r.Entries[0].Incomplete {
				t.Errorf("missing component weights accepted as complete: %+v", r.Entries[0])
			}
		})
	}
}
func TestNestedIndexErrorRetained(t *testing.T) {
	r, err := Parse(context.Background(), Input{Path: "."}, memory(map[string]string{"modules.json": `[{"path":"0_Transformer","type":"sentence_transformers.models.Transformer"}]`, "0_Transformer/config.json": `{"model_type":"bert"}`, "0_Transformer/model.safetensors.index.json": `{"weight_map":`, "0_Transformer/model.safetensors": "weights"}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range r.Issues {
		if (i.Code == "metadata_unreadable" || i.Code == "invalid_metadata") && i.Path == "0_Transformer/model.safetensors.index.json" && strings.Contains(i.Detail, "EOF") {
			found = true
		}
	}
	if !found {
		t.Errorf("malformed checkpoint index cause lost: %+v", r)
	}
	if len(r.Entries) != 1 || !r.Entries[0].Incomplete || len(r.Entries[0].Missing) != 0 {
		t.Errorf("existing directory misreported as missing: %+v", r)
	}
}
