package modelmetadata

import (
	"context"
	"strings"
	"testing"
)

// go test ./pkg/model/modelmetadata -run 'TestRecognitionDiagnostic' -count=1
func TestRecognitionDiagnosticReasons(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"transformers missing identity", map[string]string{"config.json": `{}`, "model.safetensors": "weights"}, []string{"config.json", "model_type", "architectures"}},
		{"transformers component config", map[string]string{"config.json": `{"_class_name":"AudioVAE"}`, "model.safetensors": "weights"}, []string{"config.json", "component"}},
		{"transformers missing weights", map[string]string{"config.json": `{"model_type":"example"}`}, []string{"config.json", "weights"}},
		{"transformers missing config", map[string]string{"model.safetensors": "weights"}, []string{"config.json", "checkpoint"}},
		{"sentence empty modules", map[string]string{"modules.json": `[]`}, []string{"modules.json", "no modules"}},
		{"sentence module missing type", map[string]string{"modules.json": `[{"path":"encoder"}]`}, []string{"modules.json", "type"}},
		{"sentence invalid module path", map[string]string{"modules.json": `[{"path":"../outside","type":"sentence_transformers.models.Transformer"}]`}, []string{"modules.json", "../outside"}},
		{"modelscope missing task", map[string]string{"configuration.json": `{"model":{"type":"example"}}`}, []string{"configuration.json", "task"}},
		{"modelscope missing model", map[string]string{"configuration.json": `{"task":"text-generation"}`}, []string{"configuration.json", "model"}},
		{"modelscope missing weights", map[string]string{"configuration.json": `{"task":"text-generation","model":{"type":"example"}}`}, []string{"configuration.json", "weights"}},
		{"mistral missing params", map[string]string{"tekken.json": `{}`}, []string{"params.json", "Mistral"}},
		{"mistral empty params", map[string]string{"params.json": `{}`}, []string{"params.json", "empty"}},
		{"mistral missing tokenizer", map[string]string{"params.json": `{"dim":4}`}, []string{"tekken.json"}},
		{"mistral missing weights", map[string]string{"params.json": `{"dim":4}`, "tekken.json": `{}`}, []string{"consolidated", "weights"}},
		{"adapter missing identity", map[string]string{"adapter_config.json": `{}`}, []string{"adapter_config.json", "peft_type"}},
		{"adapter component", map[string]string{"adapter_config.json": `{"peft_type":"LORA","base_model_name_or_path":"org/base"}`}, []string{"component"}},
		{"onnx missing config", map[string]string{"model.onnx": "weights"}, []string{"config.json", "ONNX"}},
		{"onnx missing identity", map[string]string{"model.onnx": "weights", "config.json": `{}`}, []string{"config.json", "model_type"}},
		{"savedmodel missing variables", map[string]string{"saved_model.pb": "graph"}, []string{"variables/variables.index"}},
		{"savedmodel missing graph", map[string]string{"variables/variables.index": "index"}, []string{"saved_model.pb"}},
		{"gguf missing identity", map[string]string{"model.gguf": gguf("", "model", 0, 0)}, []string{"model.gguf", "general.architecture"}},
		{"gguf component", map[string]string{"projector.gguf": gguf("clip", "mmproj", 0, 0)}, []string{"projector.gguf", "component"}},
		{"gguf unresolvable split", map[string]string{"model.gguf": gguf("example", "model", 0, 2)}, []string{"model.gguf", "split"}},
		{"gguf invalid split count", map[string]string{"model-00001-of-00000.gguf": gguf("example", "model", 0, 0)}, []string{"model-00001-of-00000.gguf", "split"}},
		{"hub cache root", map[string]string{"blobs/a": "weights", "snapshots/revision/config.json": `{}`}, []string{"Hugging Face", "cache"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := Parse(context.Background(), Input{}, memory(tc.files))
			if err != nil {
				t.Fatal(err)
			}
			err = report.ValidateSingleEntry()
			if err == nil {
				t.Fatalf("invalid model was admitted: %+v", report)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing cause %q in %q", want, err)
				}
			}
		})
	}
}

func TestRecognitionDiagnosticDeduplicatesNestedJSONFailure(t *testing.T) {
	report, err := Parse(context.Background(), Input{Path: "nested"}, memory(map[string]string{"nested/config.json": "{"}))
	if err != nil {
		t.Fatal(err)
	}
	err = report.ValidateSingleEntry()
	if err == nil || strings.Count(err.Error(), "unexpected end of JSON input") != 1 || strings.Count(err.Error(), "nested/config.json") != 1 {
		t.Fatalf("expected one cause at the exact file: report=%+v error=%v", report, err)
	}
}

func TestRecognitionDiagnosticDoesNotInventOtherFormatFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		files  map[string]string
		format string
	}{
		{"gguf needs no config", map[string]string{"model.gguf": gguf("llama", "model", 0, 0)}, "gguf"},
		{"onnx needs no checkpoint", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx": "weights"}, "onnx"},
		{"mistral needs no transformers config", map[string]string{"params.json": `{"dim":4}`, "tekken.json": `{}`, "consolidated.safetensors": "weights"}, "mistral"},
		{"modelscope needs no transformers config", map[string]string{"configuration.json": `{"task":"text-generation","model":{"type":"example"}}`, "pytorch_model.pt": "weights"}, "modelscope"},
		{"auxiliary modelscope declaration", map[string]string{"config.json": `{"model_type":"example"}`, "configuration.json": `{"task":"text-generation"}`, "custom.safetensors": "weights"}, "transformers"},
		{"savedmodel needs no config", map[string]string{"saved_model.pb": "graph", "variables/variables.index": "index"}, "savedmodel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{}, memory(tc.files))
			if err != nil {
				t.Fatal(err)
			}
			if err := r.ValidateSingleEntry(); err != nil {
				t.Fatalf("valid %s rejected: %v", tc.format, err)
			}
			if len(r.Entries) != 1 || r.Entries[0].Format != tc.format {
				t.Fatalf("entry changed: %+v", r)
			}
			for _, issue := range r.Issues {
				if issue.Code != "unknown_task" {
					t.Errorf("unrelated format diagnostic on a valid model: %+v", issue)
				}
			}
		})
	}
}

func TestRecognitionDiagnosticEmptyReportDoesNotGuess(t *testing.T) {
	err := (Report{}).ValidateSingleEntry()
	if err == nil || !strings.Contains(err.Error(), "no detailed failure reason") {
		t.Fatalf("unexplained report: %v", err)
	}
	for _, guess := range []string{"upgrade", "config.json", "missing weights", "parent directory"} {
		if strings.Contains(err.Error(), guess) {
			t.Fatalf("invented cause %q: %v", guess, err)
		}
	}
}
