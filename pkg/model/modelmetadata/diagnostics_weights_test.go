package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestDiagnosticWeightFailuresKeepCause(t *testing.T) {
	for _, layout := range []struct {
		name, weight string
		files        map[string]string
	}{
		{"transformers loose", "model.safetensors", map[string]string{"config.json": `{"model_type":"example"}`}},
		{"transformers indexed", "part.safetensors", map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors.index.json": `{"weight_map":{"x":"part.safetensors"}}`}},
		{"modelscope", "model.safetensors", map[string]string{"configuration.json": `{"task":"text-generation","model":{"type":"example"}}`}},
		{"mistral", "consolidated.safetensors", map[string]string{"params.json": `{"dim":4}`, "tekken.json": `{}`}},
		{"component loose", "encoder/model.safetensors", map[string]string{"modules.json": `[{"path":"encoder","type":"sentence_transformers.models.Transformer"}]`, "encoder/config.json": `{}`}},
		{"component indexed", "encoder/part.safetensors", map[string]string{"modules.json": `[{"path":"encoder","type":"sentence_transformers.models.Transformer"}]`, "encoder/config.json": `{}`, "encoder/model.safetensors.index.json": `{"weight_map":{"x":"part.safetensors"}}`}},
	} {
		for _, failure := range []struct {
			name, cause string
			err         error
			info        FileInfo
		}{
			{"permission", "permission denied", fs.ErrPermission, FileInfo{}},
			{"empty", "file is empty", nil, FileInfo{}},
			{"directory", "expected a file, found a directory", nil, FileInfo{IsDir: true}},
			{"read limit", "read limit", ErrReadLimit, FileInfo{}},
		} {
			t.Run(layout.name+"/"+failure.name, func(t *testing.T) {
				files := map[string]string{layout.weight: "weights"}
				for name, body := range layout.files {
					files[name] = body
				}
				v := diagnosticFaultView{FileView: memory(files), stats: map[string]error{layout.weight: failure.err}, info: map[string]FileInfo{layout.weight: failure.info}}
				r, err := Parse(context.Background(), Input{}, v)
				requireDiagnostic(t, r, err, layout.weight, failure.cause)
				if err != nil || len(r.Entries) != 1 || !r.Entries[0].Incomplete {
					t.Fatalf("weight failure changed model identity: %+v %v", r, err)
				}
				if len(r.Entries[0].Missing) != 0 {
					t.Fatalf("unreadable or invalid weight reported missing: %+v", r.Entries[0])
				}
				if strings.Count(r.ValidateSingleEntry().Error(), failure.cause) != 1 {
					t.Fatalf("weight cause repeated: %+v", r.Issues)
				}
			})
		}
	}
}

func TestDiagnosticWeightCancellation(t *testing.T) {
	for _, component := range []bool{false, true} {
		for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
			files := map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors": "weights"}
			if component {
				files["modules.json"] = `[{"path":".","type":"sentence_transformers.models.Transformer"}]`
			}
			v := diagnosticFaultView{FileView: memory(files), stats: map[string]error{"model.safetensors": failure}}
			_, err := Parse(context.Background(), Input{}, v)
			if !errors.Is(err, failure) {
				t.Fatalf("weight cancellation lost (component=%v): %v", component, err)
			}
		}
	}
}
