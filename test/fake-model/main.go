// fake-model creates a tiny, structurally valid Qwen-style repository for
// offline integration tests. Metadata remains realistic (including the real
// index total_size when --src points at a snapshot), while tensor payloads are
// deliberately tiny so tests never download gigabytes.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const declaredTotal = int64(8 << 30)
const shardPayload = int64(8 << 20)

type indexDoc struct {
	Metadata struct {
		TotalSize int64 `json:"total_size"`
	} `json:"metadata"`
	WeightMap map[string]string `json:"weight_map"`
}

func main() {
	src := flag.String("src", "", "Qwen3 repository snapshot (optional)")
	out := flag.String("out", "testdata/qwen3-4b-fake", "output repository")
	flag.Parse()
	if err := generate(*src, *out); err != nil {
		fmt.Fprintln(os.Stderr, "fake-model:", err)
		os.Exit(1)
	}
}

func generate(src, out string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	doc := indexDoc{WeightMap: map[string]string{}}
	if src != "" {
		b, err := os.ReadFile(filepath.Join(src, "model.safetensors.index.json"))
		if err != nil {
			return fmt.Errorf("read source index: %w", err)
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("decode source index: %w", err)
		}
		if doc.Metadata.TotalSize == 0 {
			return fmt.Errorf("source index has no metadata.total_size")
		}
		if err := copySmallFiles(src, out); err != nil {
			return err
		}
	} else {
		doc.Metadata.TotalSize = declaredTotal
		doc.WeightMap["model.embed_tokens.weight"] = "model-00001-of-00003.safetensors"
		doc.WeightMap["model.layers.0.self_attn.q_proj.weight"] = "model-00002-of-00003.safetensors"
		doc.WeightMap["model.norm.weight"] = "model-00003-of-00003.safetensors"
		if err := writeSyntheticMetadata(out); err != nil {
			return err
		}
	}
	for name := range doc.WeightMap {
		if name == "" {
			return fmt.Errorf("index contains an empty tensor name")
		}
	}
	assignShards(doc.WeightMap)
	// Reassign every indexed tensor to one of our three generated shards. The
	// index is intentionally retained with its original total_size, but the
	// generated files are tiny and remain parseable by pkg/safetensors.
	for i := 1; i <= 3; i++ {
		path := filepath.Join(out, fmt.Sprintf("model-%05d-of-00003.safetensors", i))
		if err := writeShard(path, shardNames(doc.WeightMap, i), shardPayload); err != nil {
			return err
		}
	}
	indexPath := filepath.Join(out, "model.safetensors.index.json")
	b, _ := json.MarshalIndent(doc, "", "  ")
	b = append(b, '\n')
	if err := os.WriteFile(indexPath, b, 0o644); err != nil {
		return err
	}
	return writeDocs(out)
}

func copySmallFiles(src, out string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".safetensors") || e.Name() == "model.safetensors.index.json" {
			continue
		}
		from, to := filepath.Join(src, e.Name()), filepath.Join(out, e.Name())
		in, err := os.Open(from)
		if err != nil {
			return err
		}
		outf, err := os.Create(to)
		if err != nil {
			_ = in.Close()
			return err
		}
		if _, err = io.Copy(outf, in); err != nil {
			_ = in.Close()
			_ = outf.Close()
			return err
		}
		if err = in.Close(); err != nil {
			_ = outf.Close()
			return err
		}
		if err = outf.Close(); err != nil {
			return err
		}
	}
	return nil
}

func writeSyntheticMetadata(out string) error {
	config := `{"architectures":["Qwen3ForCausalLM"],"model_type":"qwen3","hidden_size":2560,"num_hidden_layers":36,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"max_position_embeddings":40960,"vocab_size":151936,"torch_dtype":"bfloat16"}`
	if err := os.WriteFile(filepath.Join(out, "config.json"), []byte(config+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "tokenizer.json"), []byte(`{"version":"1.0","model":{"type":"BPE","vocab":{},"merges":[]}}`+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(out, "tokenizer_config.json"), []byte(`{"model_type":"qwen3","eos_token":"<|endoftext|>","bos_token":"<|endoftext|>"}`+"\n"), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, "generation_config.json"), []byte(`{"do_sample":true,"temperature":0.7,"top_p":0.8}`+"\n"), 0o644)
}

func shardNames(weights map[string]string, shard int) []string {
	var names []string
	for name, file := range weights {
		if file == fmt.Sprintf("model-%05d-of-00003.safetensors", shard) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		names = append(names, fmt.Sprintf("__gpuworks_padding_%d", shard))
	}
	sort.Strings(names)
	return names
}

func assignShards(weights map[string]string) {
	names := make([]string, 0, len(weights))
	for name := range weights {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		weights[name] = fmt.Sprintf("model-%05d-of-00003.safetensors", i%3+1)
	}
}

func writeShard(path string, names []string, payload int64) error {
	meta := map[string]any{"__metadata__": map[string]string{"format": "pt"}}
	// Keep the first tensor name semantically useful and use a padding tensor to
	// make each complete shard exactly the requested size without inventing
	// index entries. Safetensors offsets are relative to the data section, so
	// account for the eight-byte header length and JSON header itself. U8 keeps
	// the padding byte count unconstrained by BF16's two-byte element size.
	if len(names) == 0 {
		return fmt.Errorf("shard has no tensor names")
	}
	for _, name := range names[1:] {
		meta[name] = map[string]any{"dtype": "BF16", "shape": []int64{0}, "data_offsets": []int64{0, 0}}
	}
	// Add the primary tensor after the zero-sized placeholders so its header
	// can be sized before choosing the payload length.
	meta[names[0]] = map[string]any{"dtype": "U8", "shape": []int64{0}, "data_offsets": []int64{0, 0}}
	b, _ := json.Marshal(meta)
	dataBytes := payload - 8 - int64(len(b))
	if dataBytes <= 0 {
		return fmt.Errorf("shard payload %d too small for safetensors header (%d)", payload, len(b))
	}
	// The number of digits in shape/offsets can change the header length, so
	// converge on a self-consistent payload/header pair.
	for i := 0; i < 8; i++ {
		meta[names[0]] = map[string]any{"dtype": "U8", "shape": []int64{dataBytes}, "data_offsets": []int64{0, dataBytes}}
		next, _ := json.Marshal(meta)
		nextData := payload - 8 - int64(len(next))
		b = next
		if nextData == dataBytes {
			break
		}
		dataBytes = nextData
		if dataBytes <= 0 {
			return fmt.Errorf("shard payload %d too small for safetensors header", payload)
		}
	}
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(b)))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(lenBuf[:]); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	_, err = io.CopyN(f, zeroReader{}, dataBytes)
	return err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func writeDocs(out string) error {
	const warning = "Warning: generated safetensors are tiny control-plane/download fixtures, not runnable inference weights. Do not deploy this repository to vLLM/SGLang. See DESIGN.md for the exact transformation."
	readme := "# zstackai/qwen3-4b-fake\n\n" + warning + "\n"
	// A source snapshot's README is useful metadata for reviewers. Preserve it
	// in --src mode, but put an explicit fixture warning first so it cannot be
	// mistaken for an inference-ready copy.
	if source, err := os.ReadFile(filepath.Join(out, "README.md")); err == nil && len(strings.TrimSpace(string(source))) > 0 {
		readme = warning + "\n\n" + string(source)
		if !strings.HasSuffix(readme, "\n") {
			readme += "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(out, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	design := "# Design\n\nThis fixture is for control-plane/download tests, not inference. The source index metadata.total_size is retained, while three valid safetensors shards are regenerated at about 8 MiB each. Tensor names are retained, sorted, and reassigned round-robin in the rewritten index. The first tensor in each shard is a zero-filled U8 payload; additional names are zero-sized BF16 placeholders (shape [0], offsets [0,0]). With --src, small auxiliary files are copied but all safetensors data and the index mapping are still regenerated.\n"
	return os.WriteFile(filepath.Join(out, "DESIGN.md"), []byte(design), 0o644)
}
