package modelmetadata

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

type memoryView struct {
	tree      fstest.MapFS
	readBytes int64
}

func memory(files map[string]string) *memoryView {
	v := &memoryView{tree: fstest.MapFS{}}
	for name, body := range files {
		v.tree[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return v
}
func (v *memoryView) ReadDir(ctx context.Context, name string) ([]FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	list, err := fs.ReadDir(v.tree, name)
	if err != nil {
		return nil, err
	}
	var out []FileInfo
	for _, entry := range list {
		i, err := entry.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, FileInfo{Name: i.Name(), IsDir: i.IsDir(), Size: i.Size()})
	}
	return out, nil
}
func (v *memoryView) Stat(ctx context.Context, name string) (FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return FileInfo{}, err
	}
	i, err := fs.Stat(v.tree, name)
	if err != nil {
		return FileInfo{}, err
	}
	return FileInfo{Name: i.Name(), IsDir: i.IsDir(), Size: i.Size()}, nil
}
func (v *memoryView) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, ok := v.tree[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	b := f.Data
	if offset < 0 || length < 0 {
		return nil, fs.ErrInvalid
	}
	v.readBytes += length
	if offset >= int64(len(b)) {
		return nil, io.EOF
	}
	if length > int64(len(b))-offset {
		return b[offset:], io.EOF
	}
	return b[offset : offset+length], nil
}

func TestMainstreamLayouts(t *testing.T) {
	for _, tt := range []struct {
		name, format string
		files        map[string]string
		incomplete   bool
	}{
		{"transformers safetensors", "transformers", map[string]string{"config.json": `{"model_type":"bert"}`, "model.safetensors": "weights"}, false},
		{"transformers bin", "transformers", map[string]string{"config.json": `{"architectures":["BertModel"]}`, "pytorch_model.bin": "weights"}, false},
		{"indexed nested shards", "transformers", map[string]string{"config.json": `{"model_type":"qwen3"}`, "model.safetensors.index.json": `{"weight_map":{"a":"weights/a.safetensors","b":"weights/b.safetensors"}}`, "weights/a.safetensors": "a", "weights/b.safetensors": "b"}, false},
		{"missing shard cannot use other weights", "transformers", map[string]string{"config.json": `{"model_type":"qwen3"}`, "model.safetensors.index.json": `{"weight_map":{"a":"absent.safetensors"}}`, "model.safetensors": "other"}, true},
		{"sentence transformer", "sentence-transformers", map[string]string{"config.json": `{"model_type":"bert"}`, "model.safetensors": "weights", "modules.json": `[{"path":"","type":"sentence_transformers.models.Transformer"},{"path":"1_Pooling","type":"sentence_transformers.models.Pooling"}]`, "1_Pooling/config.json": "{}"}, false},
		{"diffusers", "diffusers", map[string]string{"model_index.json": `{"_class_name":"FluxPipeline","transformer":["diffusers","FluxTransformer2DModel"],"optional":[null,null]}`, "transformer/config.json": `{"_class_name":"FluxTransformer2DModel"}`, "transformer/diffusion_pytorch_model.safetensors": "weights"}, false},
		{"modular external reference", "modular-diffusers", map[string]string{"modular_model_index.json": `{"_class_name":"ExamplePipeline","encoder":["transformers","Encoder",{"repo":"org/encoder","subfolder":"encoder"}]}`}, true},
		{"modelscope", "modelscope", map[string]string{"configuration.json": `{"task":"text-ranking","model":{"type":"reranker"}}`, "pytorch_model.pt": "weights"}, false},
		{"native mistral", "mistral", map[string]string{"params.json": `{"dim":4096}`, "tekken.json": "{}", "consolidated.safetensors": "weights"}, false},
		{"onnx export", "onnx", map[string]string{"config.json": `{"model_type":"bert"}`, "model.onnx": "onnx protobuf"}, false},
		{"savedmodel export", "savedmodel", map[string]string{"saved_model.pb": "protobuf", "variables/variables.index": "index", "variables/variables.data-00000-of-00001": "variables"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{Path: "."}, memory(tt.files))
			if err != nil || len(r.Entries) != 1 || r.Entries[0].Format != tt.format || r.Entries[0].Incomplete != tt.incomplete {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

func TestComponentsAndUnmatchedLayouts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		files     map[string]string
		component bool
	}{
		{"config only", map[string]string{"config.json": `{"model_type":"bert"}`}, false},
		{"parent with descendant weights", map[string]string{"config.json": `{"model_type":"bert"}`, "child/model.safetensors": "weights"}, false},
		{"component config", map[string]string{"config.json": `{"_class_name":"AudioVAE","auto_map":{"AutoModel":"module.AudioVAE"}}`, "model.safetensors": "weights"}, false},
		{"adapter", map[string]string{"adapter_config.json": `{"peft_type":"LORA","base_model_name_or_path":"org/base"}`, "adapter_model.safetensors": "weights"}, true},
		{"random binary", map[string]string{"weights.bin": "weights", "file.pt": "weights", "random.onnx": "bytes"}, false},
		{"bad gguf", map[string]string{"model.gguf": "not GGUF"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Parse(context.Background(), Input{Path: "."}, memory(tt.files))
			if err != nil || len(r.Entries) != 0 || r.Component != tt.component {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
	v := memory(map[string]string{"audio/config.json": `{"model_type":"bert"}`, "audio/model.safetensors": "weights"})
	r, err := Parse(context.Background(), Input{Path: "audio", Context: Context{Repository: true, Components: []Reference{{Path: "audio", Role: "component"}}}}, v)
	if err != nil || !r.Component || len(r.Entries) != 0 {
		t.Fatalf("declared component registered: %+v %v", r, err)
	}
}

func TestRulesReadTheirOwnEvidence(t *testing.T) {
	extra := ruleFunc(func(ctx context.Context, in Input, files FileView) (RuleResult, error) {
		b, err := readDocument(ctx, files, "new-format.manifest")
		if err != nil {
			return RuleResult{}, err
		}
		if string(b) != "entry" {
			return RuleResult{}, nil
		}
		return RuleResult{Entries: []Entry{{Path: in.Path, Format: "example"}}}, nil
	})
	p := NewParser(extra)
	r, err := p.Parse(context.Background(), Input{Path: "."}, memory(map[string]string{"new-format.manifest": "entry"}))
	if err != nil || len(r.Entries) != 1 {
		t.Fatalf("new rule needs no scanner file whitelist: %+v %v", r, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Parse(ctx, Input{Path: "."}, memory(nil)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if _, err := p.Parse(context.Background(), Input{Path: "../A"}, memory(nil)); !errors.Is(err, ErrOutsideScope) {
		t.Fatalf("scope=%v", err)
	}
}

func TestTaskPrecedenceAndUnknown(t *testing.T) {
	for _, tt := range []struct {
		name  string
		docs  Documents
		want  []string
		issue string
	}{
		{"list-valued declaration", Documents{README: "---\npipeline_tag: [text-ranking, feature-extraction]\n---\n"}, []string{"text-ranking", "feature-extraction"}, ""},
		{"ModelScope task", Documents{Configuration: `{"task":"sentence-embedding"}`}, []string{"feature-extraction"}, ""},
		{"model card precedes ModelScope configuration", Documents{Configuration: `{"task":"text-ranking"}`, README: "---\npipeline_tag: text-generation\n---\n"}, []string{"text-generation"}, ""},
		{"contradictory peer tags", Documents{README: "---\ntags: [text-generation, feature-extraction]\n---\n"}, nil, "conflicting_tasks"},
		{"unknown task retained", Documents{README: "---\npipeline_tag: unsupported-task\n---\n"}, []string{"unsupported-task"}, ""},
		{"empty stays unknown", Documents{}, nil, ""},
		{"README prose ignored", Documents{README: "This reranker performs text-generation"}, nil, ""},
		{"generation backbone overridden", Documents{Config: `{"architectures":["Qwen3ForCausalLM"]}`, README: "---\npipeline_tag: text-ranking\n---\n"}, []string{"text-ranking"}, ""},
		{"pooling module", Documents{Modules: `[{"type":"sentence_transformers.models.Pooling"}]`}, []string{"feature-extraction"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := InferTasks(tt.docs)
			if !slices.Equal(r.Tasks, tt.want) {
				t.Fatalf("tasks=%v want=%v", r.Tasks, tt.want)
			}
			if tt.issue != "" && !slices.ContainsFunc(r.Issues, func(i Issue) bool { return i.Code == tt.issue }) {
				t.Fatalf("issues=%v", r.Issues)
			}
		})
	}
}

// The second split intentionally carries no architecture: real split GGUF
// files publish full model metadata only in the first part.
func gguf(arch, kind string, splitNo, splitCount int) string {
	var b strings.Builder
	u32 := func(n uint32) { var v [4]byte; binary.LittleEndian.PutUint32(v[:], n); b.Write(v[:]) }
	u64 := func(n uint64) { var v [8]byte; binary.LittleEndian.PutUint64(v[:], n); b.Write(v[:]) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	count := 0
	if arch != "" {
		count++
	}
	if kind != "" {
		count++
	}
	if splitCount > 0 {
		count += 2
	}
	b.WriteString("GGUF")
	u32(3)
	u64(1)
	u64(uint64(count))
	if arch != "" {
		str("general.architecture")
		u32(8)
		str(arch)
	}
	if kind != "" {
		str("general.type")
		u32(8)
		str(kind)
	}
	if splitCount > 0 {
		str("split.no")
		u32(2)
		b.WriteByte(byte(splitNo))
		b.WriteByte(0)
		str("split.count")
		u32(2)
		b.WriteByte(byte(splitCount))
		b.WriteByte(0)
	}
	return b.String()
}

func TestGGUFVariantsSplitsAndComponents(t *testing.T) {
	files := map[string]string{
		"README.md":                 "---\npipeline_tag: image-text-to-text\nlibrary_name: gguf\n---\n",
		"model-00001-of-00002.gguf": gguf("qwen4exp", "model", 0, 2),
		"model-00002-of-00002.gguf": gguf("", "", 1, 2),
		"mmproj.gguf":               gguf("clip", "mmproj", 0, 0),
		"direction.gguf":            gguf("controlvector", "", 0, 0),
	}
	r, err := Parse(context.Background(), Input{Path: "."}, memory(files))
	if err != nil || len(r.Entries) != 1 || len(r.Entries[0].Files) != 2 || r.Entries[0].Incomplete || len(r.Components) != 2 {
		t.Fatalf("GGUF report=%+v err=%v", r, err)
	}
	delete(files, "model-00002-of-00002.gguf")
	r, err = Parse(context.Background(), Input{Path: "."}, memory(files))
	if err != nil || len(r.Entries) != 1 || !r.Entries[0].Incomplete || len(r.Entries[0].Missing) != 1 {
		t.Fatalf("missing shard=%+v %v", r, err)
	}
	files["another.gguf"] = gguf("llama", "model", 0, 0)
	r, err = Parse(context.Background(), Input{Path: "."}, memory(files))
	if err != nil || len(r.Entries) != 2 {
		t.Fatalf("variants merged=%+v %v", r, err)
	}
}

func TestGGUFNeverReadsTensorPayload(t *testing.T) {
	v := memory(map[string]string{"model.gguf": gguf("llama", "model", 0, 0) + strings.Repeat("X", 4<<20)})
	r, err := Parse(context.Background(), Input{Path: "."}, v)
	if err != nil || len(r.Entries) != 1 || v.readBytes > 64<<10 {
		t.Fatalf("read=%d report=%+v err=%v", v.readBytes, r, err)
	}
	v = memory(map[string]string{"config.json": strings.Repeat(" ", maxDocumentBytes+1), "model.safetensors": "weights"})
	r, err = Parse(context.Background(), Input{Path: "."}, v)
	if err != nil || len(r.Entries) != 0 || len(r.Issues) == 0 || v.readBytes != 0 {
		t.Fatalf("oversized metadata was read: %d %+v %v", v.readBytes, r, err)
	}
}

func TestSpecificChildTaskWinsAncestorCard(t *testing.T) {
	files := memory(map[string]string{
		"modules.json":          `[{"path":"","type":"sentence_transformers.models.Transformer"},{"path":"1_Pooling","type":"sentence_transformers.models.Pooling"}]`,
		"config.json":           `{"architectures":["ExampleForCausalLM"]}`,
		"model.safetensors":     "weights",
		"1_Pooling/config.json": "{}",
	})
	r, err := Parse(context.Background(), Input{Path: ".", Context: Context{Repository: true, Tasks: []string{"text-generation"}, TaskPriority: 100}}, files)
	if err != nil || !slices.Equal(r.Tasks, []string{"feature-extraction"}) {
		t.Fatalf("independent embedding entry inherited chat: %+v %v", r, err)
	}
}

func TestConditionalGenerationDoesNotImplyChat(t *testing.T) {
	for _, raw := range []string{
		`{"architectures":["WhisperForConditionalGeneration"]}`,
		`{"architectures":["Qwen3ASRForConditionalGeneration"],"audio_config":{"model_type":"whisper"}}`,
	} {
		if r := InferTasks(Documents{Config: raw}); len(r.Tasks) != 0 {
			t.Fatalf("speech backbone became chat: %+v", r)
		}
	}
	r := InferTasks(Documents{Config: `{"architectures":["Qwen2VLForConditionalGeneration"],"vision_config":{}}`})
	if !slices.Equal(r.Tasks, []string{"image-text-to-text"}) {
		t.Fatalf("vision task=%+v", r)
	}
}
