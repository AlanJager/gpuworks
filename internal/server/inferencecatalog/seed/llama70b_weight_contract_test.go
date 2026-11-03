package seed

import (
	"strings"
	"testing"
)

func TestLlama70BMetadataCountsOneCheckpointFormat(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	want := int64(141107497872)
	models := map[string]bool{
		"meta-llama/llama-3.1-70b":          false,
		"meta-llama/llama-3.1-70b-instruct": false,
		"meta-llama/llama-3.3-70b-instruct": false,
	}
	for _, model := range catalog.Models {
		found, ok := models[model.ModelID]
		if !ok {
			continue
		}
		for _, spec := range model.Specs {
			if spec.GPUs.Vendor != "nvidia" || spec.Weights.Quantization != "bf16" {
				continue
			}
			found = true
			if spec.WeightBytes != want {
				t.Errorf("%s/%s weight_bytes=%d, want one %d-byte safetensors checkpoint rather than the sum of alternate formats", model.ModelID, spec.Name, spec.WeightBytes, want)
			}
			for _, source := range spec.Weights.Sources {
				if source.Provider == "modelscope" && !strings.HasPrefix(source.RepoID, "unsloth/") {
					t.Errorf("%s/%s ModelScope source %s may include an alternate checkpoint format", model.ModelID, spec.Name, source.RepoID)
				}
			}
		}
		models[model.ModelID] = found
	}
	for modelID, found := range models {
		if !found {
			t.Errorf("%s has no BF16 NVIDIA profile", modelID)
		}
	}
}
