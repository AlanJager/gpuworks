package modelmetadata

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestRepositoryTaskEvidencePrecedence(t *testing.T) {
	for _, tt := range []struct {
		name string
		docs Documents
		want string
	}{
		{"embedding tags precede ModelScope default", Documents{README: "---\ntags: [sentence-transformers, sentence-similarity, feature-extraction]\n---\n", Configuration: `{"task":"text-generation"}`, Config: `{"architectures":["Qwen3ForCausalLM"]}`, Modules: `[{"type":"sentence_transformers.models.Pooling"}]`}, "feature-extraction"},
		{"ranking card precedes ModelScope category", Documents{README: "---\npipeline_tag: text-ranking\n---\n", Configuration: `{"task":"multi-modal-embedding"}`, Modules: `[{"type":"sentence_transformers.cross_encoder.modules.logit_score.LogitScore"}]`}, "text-ranking"},
		{"vision card precedes generation category", Documents{README: "---\npipeline_tag: image-text-to-text\n---\n", Configuration: `{"task":"text-generation"}`}, "image-text-to-text"},
		{"pooling without model card", Documents{Configuration: `{"task":"text-generation"}`, Modules: `[{"type":"sentence_transformers.sentence_transformer.modules.pooling.Pooling"}]`}, "feature-extraction"},
		{"logit scoring without model card", Documents{Configuration: `{"task":"text-generation"}`, Modules: `[{"type":"sentence_transformers.cross_encoder.modules.logit_score.LogitScore"}]`}, "text-ranking"},
		{"library tag is not a task", Documents{Configuration: `{"task":"others"}`, README: "---\ntags: [VLLM]\n---\n"}, "others"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := InferTasks(tt.docs)
			if !slices.Equal(r.Tasks, []string{tt.want}) {
				t.Fatalf("tasks=%v want=%s evidence=%+v", r.Tasks, tt.want, r.Evidence)
			}
		})
	}
}

func TestStatelessNormalizeDoesNotRequireDirectory(t *testing.T) {
	for _, module := range []string{"sentence_transformers.models.Normalize", "sentence_transformers.sentence_transformer.modules.normalize.Normalize", "custom.Normalize", "sentence_transformers.models.Dense"} {
		t.Run(module, func(t *testing.T) {
			files := memory(map[string]string{
				"config.json": `{"model_type":"bert"}`, "model.safetensors": "weights",
				"modules.json":          fmt.Sprintf(`[{"path":"","type":"sentence_transformers.models.Transformer"},{"path":"1_Pooling","type":"sentence_transformers.models.Pooling"},{"path":"2_Output","type":%q}]`, module),
				"1_Pooling/config.json": "{}",
			})
			r, err := Parse(context.Background(), Input{Path: "."}, files)
			wantIncomplete := module == "custom.Normalize" || strings.HasSuffix(module, ".Dense")
			if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete != wantIncomplete {
				t.Fatalf("report=%+v err=%v", r, err)
			}
		})
	}
}

func largeCheckpointIndex(entries int) string {
	var b strings.Builder
	b.WriteString(`{"metadata":{"total_size":123},"weight_map":{`)
	for i := 0; i < entries; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%q", fmt.Sprintf("model.layers.%d.%s.weight", i, strings.Repeat("expert.", 20)), fmt.Sprintf("weights/part-%d.safetensors", i%2))
	}
	b.WriteString("}}")
	return b.String()
}

func TestLargeCheckpointIndexPreservesShardValidation(t *testing.T) {
	body := largeCheckpointIndex(20000)
	if len(body) <= 2<<20 {
		t.Fatal("fixture must exceed the former document limit")
	}
	for _, suffix := range []string{"", " trailing garbage"} {
		files := memory(map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors.index.json": body + suffix, "weights/part-0.safetensors": "weights"})
		r, err := Parse(context.Background(), Input{Path: "."}, files)
		if err != nil {
			t.Fatal(err)
		}
		if suffix != "" {
			if len(r.Entries) != 0 {
				t.Fatalf("invalid JSON accepted: %+v", r)
			}
			continue
		}
		if len(r.Entries) != 1 || !r.Entries[0].Incomplete || !slices.Equal(r.Entries[0].Missing, []string{"weights/part-1.safetensors"}) {
			t.Fatalf("large index=%+v", r)
		}
	}
}

func TestCheckpointIndexRejectsMalformedReferences(t *testing.T) {
	for _, body := range []string{
		`{"weight_map":{"x":"../outside.safetensors"}}`,
		`{"weight_map":{"x":null}}`,
		`{"weight_map":{"x":17}}`,
		`{"weight_map":[]}`,
		`{"weight_map":{"x":"part.safetensors"}},`,
		`{"weight_map":{"x":"part.safetensors"},"weight_map":{}}`,
		`{"metadata":{"nested":[1,2`,
	} {
		r, err := Parse(context.Background(), Input{Path: "."}, memory(map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors.index.json": body, "part.safetensors": "weights"}))
		if err != nil || len(r.Entries) != 0 || len(r.Issues) == 0 {
			t.Fatalf("invalid index accepted %s: %+v %v", body, r, err)
		}
	}
}

func largeVocabularyGGUF() string {
	var b strings.Builder
	u32 := func(n uint32) { var a [4]byte; binary.LittleEndian.PutUint32(a[:], n); b.Write(a[:]) }
	u64 := func(n uint64) { var a [8]byte; binary.LittleEndian.PutUint64(a[:], n); b.Write(a[:]) }
	str := func(s string) { u64(uint64(len(s))); b.WriteString(s) }
	b.WriteString("GGUF")
	u32(3)
	u64(1)
	u64(4)
	str("tokenizer.ggml.merges")
	u32(9)
	u32(8)
	u64(150000)
	for i := 0; i < 150000; i++ {
		str(strings.Repeat("token", 12))
	}
	// Identity and split keys may follow tokenizer arrays in the GGUF KV table.
	str("general.architecture")
	u32(8)
	str("qwen4exp")
	str("split.no")
	u32(4)
	u32(0)
	str("split.count")
	u32(4)
	u32(2)
	return b.String()
}

func TestGGUFLargeVocabularyBeforeSplitMetadata(t *testing.T) {
	body := largeVocabularyGGUF()
	files := memory(map[string]string{"model-00001-of-00002.gguf": body + strings.Repeat("T", 4<<20), "model-00002-of-00002.gguf": gguf("", "", 1, 2)})
	r, err := Parse(context.Background(), Input{Path: "."}, files)
	if err != nil || len(r.Entries) != 1 || r.Entries[0].Incomplete || len(r.Entries[0].Files) != 2 {
		t.Fatalf("large GGUF=%+v err=%v", r, err)
	}
	if files.readBytes > int64(len(body))+64<<10 {
		t.Fatalf("tensor payload read: %d", files.readBytes)
	}
}

type limitedGGUFView struct{ FileView }

func (v limitedGGUFView) ReadRange(ctx context.Context, name string, offset, length int64) ([]byte, error) {
	if strings.Contains(name, "00001-of-") {
		return nil, ErrReadLimit
	}
	return v.FileView.ReadRange(ctx, name, offset, length)
}

func TestGGUFReadFailureIsNotMissingShard(t *testing.T) {
	files := limitedGGUFView{memory(map[string]string{"model-00001-of-00002.gguf": gguf("llama", "model", 0, 2), "model-00002-of-00002.gguf": gguf("", "", 1, 2)})}
	r, err := Parse(context.Background(), Input{Path: "."}, files)
	if err != nil || len(r.Entries) != 1 || !r.Entries[0].Incomplete || len(r.Entries[0].Missing) != 0 {
		t.Fatalf("existing shard reported missing: %+v %v", r, err)
	}
	if !slices.ContainsFunc(r.Issues, func(i Issue) bool { return i.Code == "metadata_read_limit" }) {
		t.Fatalf("limit reported as corrupt metadata: %+v", r.Issues)
	}
}
