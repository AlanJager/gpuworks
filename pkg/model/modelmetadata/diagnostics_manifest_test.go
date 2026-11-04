package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestDiagnosticDiffusersManifestRejections(t *testing.T) {
	for _, name := range diffusersManifestNames {
		for _, tc := range []struct{ name, body, cause string }{
			{"missing pipeline class", `{}`, "_class_name"},
			{"invalid pipeline class", `{"_class_name":4}`, "_class_name"},
			{"no components", `{"_class_name":"ExamplePipeline"}`, "no components"},
			{"only scalar settings", `{"_class_name":"ExamplePipeline","setting":true}`, "no components"},
			{"only optional components", `{"_class_name":"ExamplePipeline","encoder":[null,null]}`, "no components"},
			{"invalid library", `{"_class_name":"ExamplePipeline","encoder":[3,"Encoder"]}`, "non-empty strings"},
			{"empty class", `{"_class_name":"ExamplePipeline","encoder":["transformers",""]}`, "non-empty strings"},
			{"invalid location", `{"_class_name":"ExamplePipeline","encoder":["transformers","Encoder",42]}`, "encoder"},
			{"outside component", `{"_class_name":"ExamplePipeline","../../encoder":["transformers","BertModel"]}`, "../../encoder"},
			{"absolute component", `{"_class_name":"ExamplePipeline","/encoder":["transformers","BertModel"]}`, "/encoder"},
		} {
			t.Run(name+"/"+tc.name, func(t *testing.T) {
				r, err := Parse(context.Background(), Input{Path: "repo"}, memory(map[string]string{"repo/" + name: tc.body}))
				requireDiagnostic(t, r, err, "repo/"+name, tc.cause)
			})
		}
	}
	r, err := Parse(context.Background(), Input{}, memory(map[string]string{"model_index.json": `{"_class_name":"ExamplePipeline","encoder":["transformers","BertModel",{"repo":"org/dependency","subfolder":"encoder"}]}`}))
	requireDiagnostic(t, r, err, "org/dependency/encoder")
}

func TestDiagnosticComponentRejections(t *testing.T) {
	for _, tc := range []struct {
		name, module string
		files        map[string]string
		cause        string
	}{
		{"unknown class", "custom.Dense", nil, "custom.Dense"},
		{"missing directory", "sentence_transformers.models.Transformer", nil, "encoder"},
		{"not directory", "sentence_transformers.models.Transformer", map[string]string{"encoder": "file"}, "encoder"},
		{"missing config", "sentence_transformers.models.Pooling", map[string]string{"encoder/other": "file"}, "encoder/config.json"},
		{"invalid config", "sentence_transformers.models.Pooling", map[string]string{"encoder/config.json": "{"}, "encoder/config.json"},
		{"missing weights", "sentence_transformers.models.Transformer", map[string]string{"encoder/config.json": `{}`}, "encoder/model weights"},
		{"missing indexed weight", "sentence_transformers.models.Transformer", map[string]string{"encoder/config.json": `{}`, "encoder/model.safetensors.index.json": `{"weight_map":{"x":"missing.safetensors"}}`}, "encoder/missing.safetensors"},
		{"bad checkpoint index", "sentence_transformers.models.Transformer", map[string]string{"encoder/config.json": `{}`, "encoder/model.safetensors.index.json": "{"}, "encoder/model.safetensors.index.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"modules.json": `[{"path":"encoder","type":"` + tc.module + `"}]`}
			for name, body := range tc.files {
				files[name] = body
			}
			r, err := Parse(context.Background(), Input{}, memory(files))
			requireDiagnostic(t, r, err, tc.cause)
			if len(r.Entries) != 1 || !r.Entries[0].Incomplete {
				t.Fatalf("component rejection changed entry identity: %+v", r)
			}
		})
	}
}

func TestDiagnosticComponentReadFailures(t *testing.T) {
	for _, name := range []string{"encoder", "encoder/config.json", "encoder/model.safetensors.index.json"} {
		for _, failure := range []error{fs.ErrPermission, context.Canceled, context.DeadlineExceeded} {
			t.Run(name+"/"+failure.Error(), func(t *testing.T) {
				v := diagnosticFaultView{
					FileView: memory(map[string]string{"modules.json": `[{"path":"encoder","type":"sentence_transformers.models.Transformer"}]`, "encoder/config.json": `{}`, "encoder/model.safetensors.index.json": `{"weight_map":{"x":"weights.safetensors"}}`}),
					stats:    map[string]error{name: failure},
				}
				r, err := Parse(context.Background(), Input{}, v)
				if failure == fs.ErrPermission {
					requireDiagnostic(t, r, err, name, "permission denied")
				} else if !errors.Is(err, failure) {
					t.Fatalf("cancellation lost: %+v %v", r, err)
				}
			})
		}
	}
}

func TestDiagnosticTokenizerFileSetRejections(t *testing.T) {
	for _, tc := range []struct {
		name, class string
		files       map[string]string
		cause       string
	}{
		{"missing slow tokenizer", "CLIPTokenizer", map[string]string{"tokenizer/other": "file"}, "tokenizer/vocab.json"},
		{"missing alternative sets", "CLIPTokenizerFast", map[string]string{"tokenizer/other": "file"}, "tokenizer/tokenizer.json"},
		{"empty tokenizer", "T5Tokenizer", map[string]string{"tokenizer/spiece.model": ""}, "tokenizer/spiece.model"},
		{"directory instead of tokenizer", "T5Tokenizer", map[string]string{"tokenizer/spiece.model/child": "file"}, "tokenizer/spiece.model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"model_index.json": `{"_class_name":"ExamplePipeline","tokenizer":["transformers","` + tc.class + `"]}`}
			for name, body := range tc.files {
				files[name] = body
			}
			r, err := Parse(context.Background(), Input{}, memory(files))
			requireDiagnostic(t, r, err, tc.cause)
		})
	}
	v := diagnosticFaultView{FileView: memory(map[string]string{"model_index.json": `{"_class_name":"ExamplePipeline","tokenizer":["transformers","T5Tokenizer"]}`, "tokenizer/spiece.model": "weights"}), stats: map[string]error{"tokenizer/spiece.model": fs.ErrPermission}}
	r, err := Parse(context.Background(), Input{}, v)
	requireDiagnostic(t, r, err, "tokenizer/spiece.model", "permission denied")
}

func TestDiagnosticCompositeDoesNotIncludeOtherFormatFailures(t *testing.T) {
	files := map[string]string{"config.json": `{"model_type":"bert"}`, "modules.json": `[{"path":"encoder","type":"sentence_transformers.models.Transformer"}]`, "encoder/config.json": `{"model_type":"bert"}`, "encoder/model.safetensors": "weights"}
	r, err := Parse(context.Background(), Input{}, memory(files))
	if err != nil || r.ValidateSingleEntry() != nil {
		t.Fatalf("valid composite rejected: %+v %v", r, err)
	}
	for _, issue := range r.Issues {
		if strings.Contains(issue.Detail, "checkpoint weights") {
			t.Fatalf("unused root checkpoint rejected: %+v", r.Issues)
		}
	}
}
