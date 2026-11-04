package modelmetadata

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

func TestDiagnosticAdmissionReasons(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report Report
		want   []string
	}{
		{"component", Report{Component: true}, []string{"model component"}},
		{"multiple entries", Report{Entries: []Entry{{Path: "one", Format: "gguf", Files: []string{"one/a.gguf"}}, {Path: "two", Format: "diffusers"}}}, []string{"found 2", "gguf entry: one/a.gguf", "diffusers entry: two"}},
		{"incomplete", Report{Entries: []Entry{{Incomplete: true, Missing: []string{"shard", "shard"}}}}, []string{"missing or unresolved file: shard"}},
		{"old issue without detail", Report{Issues: []Issue{{Path: "model.gguf", Code: "invalid_gguf"}}}, []string{"model.gguf: invalid gguf"}},
		{"unknown task only", Report{Issues: []Issue{{Code: "unknown_task"}}}, []string{"no detailed failure reason"}},
		{"root cause without path", Report{Issues: []Issue{{Detail: "bad metadata"}, {Path: ".", Detail: "bad metadata"}}}, []string{"bad metadata"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireDiagnostic(t, tc.report, nil, tc.want...)
			message := tc.report.ValidateSingleEntry().Error()
			if strings.Count(message, "- missing or unresolved file: shard") > 1 || strings.Count(message, "- bad metadata") > 1 {
				t.Fatalf("duplicate cause: %s", message)
			}
		})
	}
	valid := Report{Entries: []Entry{{Path: "model"}}, Issues: []Issue{{Code: "unknown_task"}, {Code: "task_conflict", Detail: "task only"}}}
	if err := valid.ValidateSingleEntry(); err != nil {
		t.Fatalf("task warnings rejected valid entry: %v", err)
	}
}

func TestDiagnosticMetadataIssueClassification(t *testing.T) {
	for _, tc := range []struct {
		issues  []Issue
		invalid bool
	}{
		{nil, false},
		{[]Issue{{Code: unrecognizedModelIssue, Detail: "missing config"}}, false},
		{[]Issue{{Code: unrecognizedModelIssue}, {Code: "metadata_unreadable"}}, true},
		{[]Issue{{Code: "unknown_task"}}, true},
	} {
		if got := (Report{Issues: tc.issues}).HasMetadataIssues(); got != tc.invalid {
			t.Fatalf("issues=%+v got=%v", tc.issues, got)
		}
	}
	wrapped := metadataError("config.json", &fs.PathError{Op: "read", Path: "config.json", Err: fs.ErrPermission})
	i := issue(".", "metadata_unreadable", wrapped)
	if i.Path != "config.json" || i.Detail != fs.ErrPermission.Error() {
		t.Fatalf("nested path repeated: %+v", i)
	}
}

func TestDiagnosticAdditionalDiffusersManifestFailures(t *testing.T) {
	for _, tc := range []struct{ name, body, cause string }{
		{"syntax", `{`, "unexpected end"},
		{"class", `{}`, "_class_name"},
		{"library", `{"_class_name":"Pipeline","encoder":[0,"Model"]}`, "non-empty strings"},
		{"location", `{"_class_name":"Pipeline","encoder":["transformers","Model",42]}`, "cannot unmarshal"},
		{"outside path", `{"_class_name":"Pipeline","encoder":["transformers","Model",{"subfolder":"../../outside"}]}`, "within the model directory"},
		{"same path", `{"_class_name":"Pipeline","encoder":["transformers","Model",{"subfolder":"."}]}`, "within the model directory"},
		{"sibling path", `{"_class_name":"Pipeline","encoder":["transformers","Model",{"subfolder":"../sibling"}]}`, "within the model directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{Path: "repo"}, memory(map[string]string{
				"repo/modular_model_index.json": `{"_class_name":"Pipeline","encoder":["transformers","Unknown"]}`,
				"repo/model_index.json":         tc.body,
			}))
			requireDiagnostic(t, r, err, "repo/model_index.json", tc.cause)
		})
	}
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		v := diagnosticFaultView{FileView: memory(map[string]string{"modular_model_index.json": `{"_class_name":"Pipeline","encoder":["transformers","Unknown"]}`}), stats: map[string]error{"model_index.json": failure}}
		_, err := Parse(context.Background(), Input{}, v)
		if !errors.Is(err, failure) {
			t.Fatalf("additional manifest cancellation: %v", err)
		}
	}
}

func TestDiagnosticComponentCancellation(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, name := range []string{"tokenizer", "tokenizer/spiece.model"} {
			v := diagnosticFaultView{FileView: memory(map[string]string{"model_index.json": `{"_class_name":"Pipeline","tokenizer":["transformers","T5Tokenizer"]}`, "tokenizer/spiece.model": "tokens"}), stats: map[string]error{name: failure}}
			_, err := Parse(context.Background(), Input{}, v)
			if !errors.Is(err, failure) {
				t.Fatalf("component %s cancellation: %v", name, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cacheView(memory(nil)).ReadRange(ctx, "config.json", 0, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cached read cancellation: %v", err)
	}
}

func TestDiagnosticRepositoryComponentIsNotAnIndependentModel(t *testing.T) {
	in := Input{Path: "encoder", Context: Context{Repository: true, Components: []Reference{{Path: "encoder", Role: "component"}}}}
	r, err := Parse(context.Background(), in, memory(map[string]string{"encoder/config.json": `{"model_type":"example"}`, "encoder/model.safetensors": "weights"}))
	requireDiagnostic(t, r, err, "model component")
	if len(r.Entries) != 0 {
		t.Fatalf("component escaped repository ownership: %+v", r)
	}
}
