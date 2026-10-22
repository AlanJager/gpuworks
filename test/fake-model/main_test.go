package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
	"gpuworks.ai/gpuworks/pkg/safetensors"
)

func TestGenerateProducesSmallValidRepositoryWithLargeDeclaredIndex(t *testing.T) {
	out := t.TempDir()
	if err := generate("", out); err != nil {
		t.Fatal(err)
	}
	idx, err := os.Open(filepath.Join(out, "model.safetensors.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := safetensors.ParseIndexJSON(idx)
	_ = idx.Close()
	if err != nil {
		t.Fatal(err)
	}
	if info.TotalBytes != declaredTotal {
		t.Fatalf("declared total = %d, want %d", info.TotalBytes, declaredTotal)
	}
	if info.NumTensors != 3 {
		t.Fatalf("index tensors = %d, want 3", info.NumTensors)
	}
	cfgFile, err := os.Open(filepath.Join(out, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := modelconfig.Parse(cfgFile)
	_ = cfgFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ModelType != "qwen3" || cfg.NumHiddenLayers == 0 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	design, err := os.ReadFile(filepath.Join(out, "DESIGN.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(design), "round-robin") || !strings.Contains(string(design), "zero-sized BF16") {
		t.Fatalf("generated DESIGN.md does not describe the actual transformation: %s", design)
	}
	indexBytes, err := os.ReadFile(filepath.Join(out, "model.safetensors.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		WeightMap map[string]string `json:"weight_map"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i := 1; i <= 3; i++ {
		f, err := os.Open(filepath.Join(out, "model-"+fmt.Sprintf("%05d", i)+"-of-00003.safetensors"))
		if err != nil {
			t.Fatal(err)
		}
		stat, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		if stat.Size() != shardPayload {
			t.Fatalf("shard %d size=%d, want %d", i, stat.Size(), shardPayload)
		}
		header, err := safetensors.ParseHeader(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(header) == 0 {
			t.Fatalf("shard %d has no tensor header", i)
		}
		for name, meta := range header {
			seen[name]++
			if len(meta.Shape) == 0 || len(meta.Offsets) != 2 || meta.Offsets[0] < 0 || meta.Offsets[1] < meta.Offsets[0] {
				t.Fatalf("invalid generated tensor %q: %#v", name, meta)
			}
		}
	}
	if len(seen) != len(index.WeightMap) {
		t.Fatalf("generated tensor names=%d, index names=%d", len(seen), len(index.WeightMap))
	}
	for name, shard := range index.WeightMap {
		if seen[name] != 1 {
			t.Fatalf("index tensor %q appears %d times in generated shards", name, seen[name])
		}
		if shard == "" {
			t.Fatalf("index tensor %q has empty shard", name)
		}
	}
}

func TestGenerateFromSourceRewritesShardsButPreservesMetadata(t *testing.T) {
	src, out := t.TempDir(), t.TempDir()
	index := `{"metadata":{"total_size":123456789},"weight_map":{"z":"old-a.safetensors","a":"old-b.safetensors","m":"old-c.safetensors","n":"old-c.safetensors"}}` + "\n"
	if err := os.WriteFile(filepath.Join(src, "model.safetensors.index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"README.md":               []byte("source readme\n"),
		"tokenizer.json":          []byte(`{"version":"source"}`),
		"old-weights.safetensors": []byte("must not be copied\n"),
	} {
		if err := os.WriteFile(filepath.Join(src, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := generate(src, out); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(out, "config.json")); err != nil || string(got) != `{"model_type":"qwen3"}` {
		t.Fatalf("small source file not copied: %q err=%v", got, err)
	}
	for name, want := range map[string]string{
		"README.md":      "source readme\n",
		"tokenizer.json": `{"version":"source"}`,
	} {
		got, err := os.ReadFile(filepath.Join(out, name))
		if err != nil || !strings.Contains(string(got), want) {
			t.Fatalf("auxiliary source file %s = %q err=%v, want content %q", name, got, err, want)
		}
	}
	readme, err := os.ReadFile(filepath.Join(out, "README.md"))
	if err != nil || !strings.HasPrefix(string(readme), "Warning: generated safetensors") {
		t.Fatalf("source README missing fixture warning: %q err=%v", readme, err)
	}
	if _, err := os.Stat(filepath.Join(out, "old-weights.safetensors")); !os.IsNotExist(err) {
		t.Fatalf("source safetensors unexpectedly copied, err=%v", err)
	}
	var doc indexDoc
	b, err := os.ReadFile(filepath.Join(out, "model.safetensors.index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Metadata.TotalSize != 123456789 || len(doc.WeightMap) != 4 {
		t.Fatalf("source metadata changed: total=%d tensors=%d", doc.Metadata.TotalSize, len(doc.WeightMap))
	}
	shards := make([]string, 0, len(doc.WeightMap))
	for _, shard := range doc.WeightMap {
		shards = append(shards, shard)
	}
	sort.Strings(shards)
	for _, shard := range shards {
		if shard < "model-00001-of-00003.safetensors" || shard > "model-00003-of-00003.safetensors" {
			t.Fatalf("source tensor retained unexpected shard %q", shard)
		}
	}
}
