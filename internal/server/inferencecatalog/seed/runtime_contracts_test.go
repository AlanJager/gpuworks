package seed

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
)

func TestErnieImageRuntimeConfiguration(t *testing.T) {
	catalog, err := Catalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model.ModelID != "paddlepaddle/ernie-image" {
			continue
		}
		if len(model.Specs) == 0 {
			t.Fatal("ERNIE-Image has no templates")
		}
		for _, spec := range model.Specs {
			var requestTimeout string
			var config struct {
				LoadKwargs struct {
					Dtype string `json:"torch_dtype"`
				} `json:"load_kwargs"`
			}
			for index, arg := range spec.Args {
				if arg == "--request-timeout" && index+1 < len(spec.Args) {
					requestTimeout = spec.Args[index+1]
				}
				if arg == "--adapter-config" && index+1 < len(spec.Args) {
					if err := json.Unmarshal([]byte(spec.Args[index+1]), &config); err != nil {
						t.Fatal(err)
					}
				}
			}
			// K100 needs more than the wrapper's default 300 seconds for 50 steps.
			if requestTimeout != "600" {
				t.Errorf("%s does not allow the default 50-step request to finish", spec.Name)
			}
			if config.LoadKwargs.Dtype != "bfloat16" {
				t.Errorf("%s does not load the BF16 weights in BF16", spec.Name)
			}
		}
		return
	}
	t.Fatal("ERNIE-Image is missing")
}

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
				// Both capture decode graphs with a capped batch (issue #35);
				// mini also keeps prefill capture off.
				want := map[string]string{
					"inclusionai/llada2.1-flash": "--cuda-graph-max-bs-decode 16",
					"inclusionai/llada2.1-mini":  "--cuda-graph-backend-prefill disabled --cuda-graph-max-bs-decode 8",
				}[model.ModelID]
				bounded := !strings.Contains(args, "--disable-cuda-graph") && strings.Contains(args, want)
				if !bounded {
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

// LLaDA2.1-mini trades graph coverage for memory twice: prefill capture is
// off and decode capture stops at batch 8 (issue #35). The engine registry
// grades both as information, so nothing forces a reason; the profile states
// one for each anyway, and a reason must not outlive its setting.
func TestLLaDAMiniStatesWhyItNarrowsGraphCapture(t *testing.T) {
	_, spec := catalogSpec(t, "inclusionai/llada2.1-mini", "bf16-sglang-nvidia-general")
	graded := map[string]string{}
	for _, finding := range inferencecatalog.SpecPerformanceFindings(spec) {
		if finding.Cost != engines.CostGraphCaptureNarrow || finding.Level != engines.LevelInfo {
			t.Errorf("%s %s is graded %s/%s, want %s/%s", finding.Param, finding.Value, finding.Cost, finding.Level, engines.CostGraphCaptureNarrow, engines.LevelInfo)
		}
		graded[finding.Param] = finding.Value
	}
	want := map[string]string{"--cuda-graph-backend-prefill": "disabled", "--cuda-graph-max-bs-decode": "8"}
	if !reflect.DeepEqual(graded, want) {
		t.Fatalf("graded settings = %v, want %v", graded, want)
	}
	stated := map[string]inferencecatalog.PerformanceTradeoffReason{}
	for _, tradeoff := range spec.PerformanceTradeoffs {
		stated[tradeoff.Param] = tradeoff.Reason
	}
	for param := range want {
		if stated[param] != inferencecatalog.ReasonMemoryFit {
			t.Errorf("%s states reason %q, want %s", param, stated[param], inferencecatalog.ReasonMemoryFit)
		}
	}
	if len(stated) != len(want) {
		t.Errorf("performance_tradeoffs states %v, want exactly %v", stated, want)
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
