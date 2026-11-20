package packages

import (
	"testing"
	"testing/fstest"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func durablePackageFixture(t *testing.T) fstest.MapFS {
	t.Helper()
	model := inferencecatalog.InferenceModel{ModelID: "org/model", DisplayTitle: "Model", Specs: []inferencecatalog.InferenceSpec{{Name: "default", Engine: "vllm", Image: "vllm/vllm-openai:latest", Weights: inferencecatalog.Weights{Quantization: "bf16", Sources: []inferencecatalog.WeightSource{{Provider: "huggingface", RepoID: "org/model"}}}, GPUs: inferencecatalog.GPUSelector{Vendor: "nvidia", CC: ">=8.0"}}}}
	data, err := inferencecatalog.MarshalModelYAML(model)
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"manifest.yaml": {Data: []byte("schema_version: 1\npublished_at: 2026-09-21T00:00:00Z\nmodels: 1\nspecs: 1\n")}, "models/org/model/model.yaml": {Data: data}, "models/org/model/README.en.md": {Data: []byte("original documentation")}}
}
