package modelmetadata

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
)

func TestDiagnosticCheckpointIndexRejections(t *testing.T) {
	for _, tc := range []struct{ name, body, cause string }{
		{"empty document", "", "EOF"},
		{"not object", `[]`, "expected"},
		{"invalid root key", `{x:1}`, "invalid character"},
		{"broken unknown value", `{"metadata":[}`, "invalid character"},
		{"missing map", `{}`, "empty weight_map"},
		{"empty map", `{"weight_map":{}}`, "empty weight_map"},
		{"map not object", `{"weight_map":[]}`, "expected"},
		{"invalid tensor key", `{"weight_map":{x:1}}`, "invalid character"},
		{"non-string target", `{"weight_map":{"x":42}}`, "cannot unmarshal"},
		{"empty target", `{"weight_map":{"x":""}}`, "outside"},
		{"absolute target", `{"weight_map":{"x":"/weights"}}`, "outside"},
		{"outside target", `{"weight_map":{"x":"../weights"}}`, "outside"},
		{"backslash target", `{"weight_map":{"x":"a\\b"}}`, "outside"},
		{"truncated map", `{"weight_map":{"x":"a"`, "EOF"},
		{"truncated root", `{"weight_map":{"x":"a"}`, "EOF"},
		{"duplicate map", `{"weight_map":{"x":"a"},"weight_map":{}}`, "duplicate weight_map"},
		{"trailing value", `{"weight_map":{"x":"a"}} 1`, "data after checkpoint index"},
		{"trailing garbage", `{"weight_map":{"x":"a"}} !`, "invalid character"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, component := range []bool{false, true} {
				files := map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors.index.json": tc.body}
				if component {
					files["modules.json"] = `[{"path":".","type":"sentence_transformers.models.Transformer"}]`
				}
				r, err := Parse(context.Background(), Input{}, memory(files))
				requireDiagnostic(t, r, err, "model.safetensors.index.json", tc.cause)
			}
		})
	}
}

func TestDiagnosticCheckpointIndexIOFailures(t *testing.T) {
	const index = "model.safetensors.index.json"
	for _, tc := range []struct {
		name  string
		view  diagnosticFaultView
		cause string
	}{
		{"disappeared index", diagnosticFaultView{stats: map[string]error{index: fs.ErrNotExist}}, "does not exist"},
		{"unreadable index", diagnosticFaultView{reads: map[string]error{index: fs.ErrPermission}}, "permission denied"},
		{"stat failure", diagnosticFaultView{stats: map[string]error{index: fs.ErrPermission}}, "permission denied"},
		{"index directory", diagnosticFaultView{info: map[string]FileInfo{index: {IsDir: true}}}, "invalid"},
		{"oversized index", diagnosticFaultView{info: map[string]FileInfo{index: {Size: maxCheckpointIndexBytes + 1}}}, "read limit"},
		{"truncated index", diagnosticFaultView{reads: map[string]error{index: io.ErrUnexpectedEOF}}, "unexpected EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.view.FileView = memory(map[string]string{"config.json": `{"model_type":"example"}`, index: `{"weight_map":{"x":"a"}}`})
			r, err := Parse(context.Background(), Input{}, tc.view)
			requireDiagnostic(t, r, err, index, tc.cause)
		})
	}
}

func TestDiagnosticCheckpointShardRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		weights map[string]string
		cause   string
	}{
		{"ambiguous indexes", map[string]string{"a.safetensors.index.json": `{"weight_map":{"x":"a"}}`, "b.safetensors.index.json": `{"weight_map":{"x":"b"}}`}, "ambiguous checkpoint indexes"},
		{"zero shard", map[string]string{"custom-00000-of-00002.bin": "weights"}, "invalid checkpoint shard number"},
		{"part beyond count", map[string]string{"custom-00003-of-00002.pt": "weights"}, "invalid checkpoint shard number"},
		{"zero count", map[string]string{"custom-00001-of-00000.safetensors": "weights"}, "invalid checkpoint shard number"},
		{"inconsistent count", map[string]string{"custom-00001-of-00002.safetensors": "weights", "custom-00002-of-00003.safetensors": "weights"}, "inconsistent checkpoint shard counts"},
		{"too many shards", map[string]string{"custom-00001-of-10001.safetensors": "weights"}, "read limit"},
		{"too many combined shards", map[string]string{"a-00001-of-06000.safetensors": "weights", "b-00001-of-06000.safetensors": "weights"}, "read limit"},
		{"missing shard", map[string]string{"custom-00001-of-00002.safetensors": "weights"}, "custom-00002-of-00002.safetensors"},
		{"empty shard", map[string]string{"custom-00001-of-00001.safetensors": ""}, "custom-00001-of-00001.safetensors"},
		{"missing indexed shard", map[string]string{"custom.safetensors.index.json": `{"weight_map":{"x":"missing.safetensors"}}`}, "missing.safetensors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.weights["config.json"] = `{"model_type":"example"}`
			r, err := Parse(context.Background(), Input{}, memory(tc.weights))
			requireDiagnostic(t, r, err, tc.cause)
		})
	}
	for _, filename := range []string{"model-00001-of-10001.safetensors", "model-00001-of-00002.safetensors"} {
		r, err := Parse(context.Background(), Input{}, memory(map[string]string{"modules.json": `[{"path":".","type":"sentence_transformers.models.Transformer"}]`, "config.json": `{}`, filename: "weights"}))
		cause := "read limit"
		if strings.Contains(filename, "00002") {
			cause = "model-00002-of-00002.safetensors"
		}
		requireDiagnostic(t, r, err, cause)
	}
	r, err := Parse(context.Background(), Input{}, memory(map[string]string{"configuration.json": `{"task":"text-generation","model":{"type":"example"}}`, "weights.safetensors.index.json": "{"}))
	requireDiagnostic(t, r, err, "weights.safetensors.index.json", "EOF")
}

func TestDiagnosticRuleDirectoryFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rule  ruleFunc
		files map[string]string
	}{
		{"repository", parseRepository, nil},
		{"transformers without config", parseTransformers, nil},
		{"transformers weights", parseTransformers, map[string]string{"config.json": `{"model_type":"example"}`}},
		{"mistral without params", parseMistral, nil},
		{"mistral weights", parseMistral, map[string]string{"params.json": `{"dim":4}`, "tekken.json": `{}`}},
		{"gguf", parseGGUF, nil},
		{"component weights", parseSentenceTransformers, map[string]string{"modules.json": `[{"path":"encoder","type":"sentence_transformers.models.Transformer"}]`, "encoder/config.json": `{}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := diagnosticFaultView{FileView: memory(tc.files), dirs: map[string]error{".": fs.ErrPermission, "encoder": fs.ErrPermission}}
			r, err := tc.rule(context.Background(), Input{Path: "."}, v)
			requireDiagnostic(t, Report{Entries: r.Entries, Issues: r.Issues}, err, "permission denied")
		})
	}
}

func TestDiagnosticIndexReaderFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := fileRangeReader{ctx: ctx}
	if _, err := r.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r = fileRangeReader{ctx: context.Background(), files: diagnosticShortReadView{FileView: memory(nil)}, name: "index", size: 2}
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero read: %d %v", n, err)
	}
	if _, err := r.Read(make([]byte, 2)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short read: %v", err)
	}
}

type diagnosticShortReadView struct{ FileView }

func (diagnosticShortReadView) ReadRange(context.Context, string, int64, int64) ([]byte, error) {
	return []byte("x"), nil
}
