package modelmetadata

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

type diagnosticFaultView struct {
	FileView
	dirs  map[string]error
	stats map[string]error
	reads map[string]error
	info  map[string]FileInfo
}

func (v diagnosticFaultView) ReadDir(ctx context.Context, name string) ([]FileInfo, error) {
	if err := v.dirs[name]; err != nil {
		return nil, err
	}
	return v.FileView.ReadDir(ctx, name)
}

func (v diagnosticFaultView) Stat(ctx context.Context, name string) (FileInfo, error) {
	if err := v.stats[name]; err != nil {
		return FileInfo{}, err
	}
	if info, ok := v.info[name]; ok {
		return info, nil
	}
	return v.FileView.Stat(ctx, name)
}

func (v diagnosticFaultView) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if err := v.reads[name]; err != nil {
		return nil, err
	}
	return v.FileView.ReadRange(ctx, name, offset, length)
}

func requireDiagnostic(t *testing.T, report Report, err error, contains ...string) {
	t.Helper()
	if err == nil {
		err = report.ValidateSingleEntry()
	}
	if err == nil {
		t.Fatalf("expected rejection: %+v", report)
	}
	for _, part := range contains {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("cause %q missing from %q", part, err)
		}
	}
}

func TestDiagnosticMetadataReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		view  diagnosticFaultView
		cause string
	}{
		{"stat denied", diagnosticFaultView{stats: map[string]error{"model/config.json": fs.ErrPermission}}, "permission denied"},
		{"read denied", diagnosticFaultView{reads: map[string]error{"model/config.json": fs.ErrPermission}}, "permission denied"},
		{"read outside scope", diagnosticFaultView{reads: map[string]error{"model/config.json": ErrOutsideScope}}, "outside"},
		{"read truncated", diagnosticFaultView{reads: map[string]error{"model/config.json": io.ErrUnexpectedEOF}}, "unexpected EOF"},
		{"document limit", diagnosticFaultView{info: map[string]FileInfo{"model/config.json": {Name: "config.json", Size: maxDocumentBytes + 1}}}, "read limit"},
		{"directory instead of document", diagnosticFaultView{info: map[string]FileInfo{"model/config.json": {Name: "config.json", IsDir: true}}}, "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.view.FileView = memory(map[string]string{"model/config.json": `{"model_type":"example"}`, "model/model.safetensors": "weights"})
			report, err := Parse(context.Background(), Input{Path: "model"}, tc.view)
			requireDiagnostic(t, report, err, "model/config.json", tc.cause)
		})
	}
}

func TestDiagnosticJSONFailuresKeepFileAndCause(t *testing.T) {
	for _, name := range []string{"config.json", "configuration.json", "modules.json", "params.json", "adapter_config.json", "model_index.json", "modular_model_index.json"} {
		t.Run(name, func(t *testing.T) {
			report, err := Parse(context.Background(), Input{Path: "nested"}, memory(map[string]string{"nested/" + name: "{"}))
			requireDiagnostic(t, report, err, "nested/"+name, "unexpected end of JSON input")
			if err == nil && strings.Count(report.ValidateSingleEntry().Error(), "unexpected end of JSON input") != 1 {
				t.Fatalf("same JSON failure repeated: %+v", report.Issues)
			}
		})
	}
	for _, body := range []string{`{"model_type":42}`, `{"architectures":"Example"}`} {
		r, err := Parse(context.Background(), Input{}, memory(map[string]string{"config.json": body}))
		requireDiagnostic(t, r, err, "config.json", "cannot unmarshal")
	}
}

func TestDiagnosticParserInputAndCancellation(t *testing.T) {
	for _, path := range []string{"../outside", "/absolute", "a//b"} {
		_, err := Parse(context.Background(), Input{Path: path}, memory(nil))
		if !errors.Is(err, ErrOutsideScope) {
			t.Errorf("path=%q err=%v", path, err)
		}
	}
	for _, failure := range []error{fs.ErrPermission, fs.ErrNotExist, ErrReadLimit, context.Canceled, context.DeadlineExceeded} {
		_, err := Parse(context.Background(), Input{}, diagnosticFaultView{FileView: memory(nil), dirs: map[string]error{".": failure}})
		if !errors.Is(err, failure) {
			t.Errorf("root error lost: want=%v got=%v", failure, err)
		}
	}
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		p := NewParser(ruleFunc(func(context.Context, Input, FileView) (RuleResult, error) { return RuleResult{}, failure }))
		_, err := p.Parse(context.Background(), Input{}, memory(nil))
		if !errors.Is(err, failure) {
			t.Errorf("rule cancellation lost: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewParser(ruleFunc(func(context.Context, Input, FileView) (RuleResult, error) { cancel(); return RuleResult{}, nil }))
	_, err := p.Parse(ctx, Input{}, memory(nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation lost: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	p = NewParser(
		ruleFunc(func(context.Context, Input, FileView) (RuleResult, error) { cancel(); return RuleResult{}, nil }),
		ruleFunc(func(context.Context, Input, FileView) (RuleResult, error) {
			t.Fatal("ran a rule after cancellation")
			return RuleResult{}, nil
		}),
	)
	_, err = p.Parse(ctx, Input{}, memory(nil))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation between rules lost: %v", err)
	}
}

func TestDiagnosticEachRuleCanBeInapplicable(t *testing.T) {
	for _, rule := range []struct {
		name  string
		parse ruleFunc
	}{
		{"repository", parseRepository}, {"transformers", parseTransformers},
		{"sentence transformers", parseSentenceTransformers}, {"diffusers", parseDiffusers},
		{"modelscope", parseModelScope}, {"mistral", parseMistral},
		{"gguf", parseGGUF}, {"adapter", parseAdapter}, {"exports", parseExports},
	} {
		t.Run(rule.name, func(t *testing.T) {
			r, err := rule.parse(context.Background(), Input{Path: "."}, memory(map[string]string{"notes.txt": "not model metadata"}))
			if err != nil || len(r.Entries) != 0 || len(r.Issues) != 0 {
				t.Fatalf("inapplicable format reported a failure: %+v %v", r, err)
			}
		})
	}
}

func TestDiagnosticREADMEReadFailures(t *testing.T) {
	for _, name := range []string{"README.md", "readme.MD"} {
		v := diagnosticFaultView{FileView: memory(map[string]string{name: "---\npipeline_tag: text-generation\n---\n"}), reads: map[string]error{name: fs.ErrPermission}}
		r, err := Parse(context.Background(), Input{}, v)
		requireDiagnostic(t, r, err, name, "permission denied")
	}
	for _, body := range []string{"---\npipeline_tag: text-generation", "---\ntags: [\n---\n"} {
		r, err := Parse(context.Background(), Input{}, memory(map[string]string{"README.md": body}))
		requireDiagnostic(t, r, err, "README.md")
	}
}

func TestDiagnosticRequiredFilesReportActualFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"tekken.json", map[string]string{"params.json": `{"dim":4}`}},
		{"saved_model.pb", map[string]string{"variables/variables.index": "index"}},
		{"variables/variables.index", map[string]string{"saved_model.pb": "graph"}},
	} {
		for _, failure := range []struct {
			name  string
			info  FileInfo
			err   error
			cause string
		}{
			{"missing", FileInfo{}, fs.ErrNotExist, "does not exist"},
			{"unreadable", FileInfo{}, fs.ErrPermission, "permission denied"},
			{"empty", FileInfo{}, nil, "file is empty"},
			{"directory", FileInfo{IsDir: true}, nil, "expected a file, found a directory"},
		} {
			t.Run(tc.name+"/"+failure.name, func(t *testing.T) {
				v := diagnosticFaultView{FileView: memory(tc.files), stats: map[string]error{tc.name: failure.err}, info: map[string]FileInfo{tc.name: failure.info}}
				r, err := Parse(context.Background(), Input{}, v)
				requireDiagnostic(t, r, err, tc.name, failure.cause)
			})
		}
	}
}
