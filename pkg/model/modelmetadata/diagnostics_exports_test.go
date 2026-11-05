package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestDiagnosticExportFailuresKeepCause(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		view  diagnosticFaultView
		want  []string
	}{
		{"empty ONNX", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx": ""}, diagnosticFaultView{}, []string{"model.onnx", "file is empty"}},
		{"ONNX directory", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx/part": "data"}, diagnosticFaultView{}, []string{"model.onnx", "expected a file, found a directory"}},
		{"ONNX stat denied", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx": "weights"}, diagnosticFaultView{stats: map[string]error{"model.onnx": fs.ErrPermission}}, []string{"model.onnx", "permission denied"}},
		{"ONNX lost during scan", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx": "weights"}, diagnosticFaultView{stats: map[string]error{"model.onnx": fs.ErrNotExist}}, []string{"model.onnx", "does not exist"}},
		{"ONNX empty without config", map[string]string{"model.onnx": ""}, diagnosticFaultView{}, []string{"model.onnx", "file is empty", "config.json", "ONNX"}},
		{"SavedModel both empty", map[string]string{"saved_model.pb": "", "variables/variables.index": ""}, diagnosticFaultView{}, []string{"saved_model.pb", "variables/variables.index", "file is empty"}},
		{"SavedModel graph empty variables absent", map[string]string{"saved_model.pb": ""}, diagnosticFaultView{}, []string{"saved_model.pb", "file is empty", "variables/variables.index", "does not exist"}},
		{"SavedModel variables empty graph absent", map[string]string{"variables/variables.index": ""}, diagnosticFaultView{}, []string{"saved_model.pb", "does not exist", "variables/variables.index", "file is empty"}},
		{"SavedModel both denied", map[string]string{"saved_model.pb": "graph", "variables/variables.index": "index"}, diagnosticFaultView{stats: map[string]error{"saved_model.pb": fs.ErrPermission, "variables/variables.index": fs.ErrPermission}}, []string{"saved_model.pb", "variables/variables.index", "permission denied"}},
		{"SavedModel graph lost variables absent", map[string]string{"saved_model.pb": "graph"}, diagnosticFaultView{stats: map[string]error{"saved_model.pb": fs.ErrNotExist}}, []string{"saved_model.pb", "does not exist"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.view.FileView = memory(tc.files)
			r, err := Parse(context.Background(), Input{}, tc.view)
			requireDiagnostic(t, r, err, tc.want...)
			if err != nil || len(r.Entries) != 0 {
				t.Fatalf("invalid export admitted: %+v %v", r, err)
			}
		})
	}
}

func TestDiagnosticExportsStayInapplicableWithoutEvidence(t *testing.T) {
	for _, files := range []map[string]string{
		{"config.json": `{"model_type":"example"}`},
		{"config.json": `{"model_type":"example"}`, "model.safetensors": "weights"},
		{"variables/notes.txt": "not a SavedModel"},
	} {
		r, err := parseExports(context.Background(), Input{Path: "."}, memory(files))
		if err != nil || len(r.Entries) != 0 || len(r.Issues) != 0 {
			t.Fatalf("unrelated export failure: %+v %v", r, err)
		}
	}
}

func TestDiagnosticExportFailureDoesNotRejectAnotherFormat(t *testing.T) {
	r, err := Parse(context.Background(), Input{}, memory(map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors": "weights", "model.onnx": "", "saved_model.pb": ""}))
	if err != nil || r.ValidateSingleEntry() != nil {
		t.Fatalf("valid checkpoint rejected: %+v %v", r, err)
	}
	for _, i := range r.Issues {
		if strings.Contains(i.Path, "onnx") || strings.Contains(i.Path, "saved_model") {
			t.Fatalf("unused export issue leaked: %+v", i)
		}
	}
}

func TestDiagnosticExportCancellation(t *testing.T) {
	for _, name := range []string{"model.onnx", "saved_model.pb", "variables/variables.index"} {
		for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
			v := diagnosticFaultView{FileView: memory(map[string]string{"config.json": `{"model_type":"example"}`, name: "data"}), stats: map[string]error{name: failure}}
			_, err := Parse(context.Background(), Input{}, v)
			if !errors.Is(err, failure) {
				t.Fatalf("%s cancellation lost: %v", name, err)
			}
		}
	}
}
