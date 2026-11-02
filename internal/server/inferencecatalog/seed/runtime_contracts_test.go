package seed

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

func TestLLaDAA100UsesTheCompatibleMoERouter(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{
		"inclusionai/llada2.1-flash": false,
		"inclusionai/llada2.1-mini":  false,
	}
	for _, model := range catalog.Models {
		if _, ok := wanted[model.ModelID]; !ok {
			continue
		}
		wanted[model.ModelID] = true
		for _, spec := range model.Specs {
			if spec.Name == "bf16-sglang-nvidia-general" &&
				spec.Env["SGLANG_OPT_USE_JIT_KERNEL_GROUPED_TOPK"] != "1" {
				t.Errorf("%s A100 profile selects the unsupported FlashInfer fused-top-k kernel", model.ModelID)
			}
			if spec.Name == "bf16-sglang-nvidia-general" {
				args := strings.Join(spec.Args, " ")
				if !strings.Contains(args, "--disable-cuda-graph") {
					t.Errorf("%s A100 profile must avoid oversized CUDA graph capture", model.ModelID)
				}
			}
		}
	}
	for modelID, seen := range wanted {
		if !seen {
			t.Errorf("%s is missing from the catalog", modelID)
		}
	}
}

func TestVideoGenerationTemplatesUseMultipart(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			if spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type == inferenceapi.OperationVideoGenerate &&
					operation.RequestEncoding != inferenceapi.EncodingMultipart {
					t.Errorf("%s %s sends %s through %q",
						model.ModelID,
						spec.Name,
						operation.Type,
						operation.RequestEncoding,
					)
				}
			}
		}
	}
}

func TestLongCatImageEditExposesTheEditEndpoint(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "meituan-longcat/longcat-image-edit" {
			continue
		}
		for _, spec := range model.Specs {
			if spec.API == nil {
				continue
			}
			for _, operation := range spec.API.Operations {
				if operation.Type == inferenceapi.OperationImageEdit {
					if operation.Endpoint != "/v1/images/edits" ||
						operation.RequestEncoding != inferenceapi.EncodingMultipart {
						t.Errorf("%s image edit operation = %+v", spec.Name, operation)
					}
					return
				}
			}
		}
		t.Fatal("LongCat image edit operation is missing")
	}
	t.Fatal("LongCat image edit model is missing")
}
