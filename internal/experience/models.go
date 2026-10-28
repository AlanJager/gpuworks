package experience

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
)

// shardBytes is the size of one weight shard, as published checkpoints split.
const shardBytes = int64(5) << 30

// modelShape is what a simulated checkpoint needs: the config.json the
// estimator reads, and the weight size the scheduler and disk checks use.
type modelShape struct {
	Config      map[string]any
	WeightBytes int64
	// Readme is the catalog's model card, served as the repository README.
	Readme string
}

// catalogShapes indexes the built-in inference catalog by weight repository,
// so a simulated checkpoint has the real model's config.json and size.
func catalogShapes() (map[string]modelShape, error) {
	catalog, err := seed.Catalog()
	if err != nil {
		return nil, err
	}
	shapes := map[string]modelShape{}
	for _, model := range catalog.Models {
		if model.ModelConfig == nil {
			continue
		}
		raw, err := json.Marshal(model.ModelConfig)
		if err != nil {
			continue
		}
		var config map[string]any
		if json.Unmarshal(raw, &config) != nil {
			continue
		}
		dropEmpty(config)
		if _, ok := config["architectures"]; !ok {
			config["architectures"] = []string{architectureFor(model)}
		}
		if model.ContextLength > 0 {
			if _, ok := config["max_position_embeddings"]; !ok {
				config["max_position_embeddings"] = model.ContextLength
			}
		}
		for _, spec := range model.Specs {
			if spec.WeightBytes <= 0 {
				continue
			}
			for _, source := range spec.Weights.Sources {
				key := strings.ToLower(source.RepoID)
				if _, seen := shapes[key]; !seen && key != "" {
					shapes[key] = modelShape{Config: config, WeightBytes: spec.WeightBytes, Readme: model.ReadmeZHCN}
				}
			}
		}
	}
	// A repository named without its organization (an HTTP directory's
	// model, a local directory) finds the same shape by its last segment.
	for key, shape := range shapes {
		if i := strings.LastIndex(key, "/"); i >= 0 {
			if _, taken := shapes[key[i+1:]]; !taken {
				shapes[key[i+1:]] = shape
			}
		}
	}
	return shapes, nil
}

// dropEmpty removes the zero values a typed config marshals, which a real
// config.json would simply not contain.
func dropEmpty(config map[string]any) {
	for key, value := range config {
		switch v := value.(type) {
		case nil:
			delete(config, key)
		case float64:
			if v == 0 {
				delete(config, key)
			}
		case string:
			if v == "" {
				delete(config, key)
			}
		case []any:
			if len(v) == 0 {
				delete(config, key)
			}
		case map[string]any:
			if len(v) == 0 {
				delete(config, key)
			}
		}
	}
}

func architectureFor(model inferencecatalog.InferenceModel) string {
	if strings.EqualFold(model.Architecture, "moe") {
		return "Qwen3MoeForCausalLM"
	}
	return "Qwen3ForCausalLM"
}

// fallbackShape stands in for a repository the catalog does not know: a
// typical 7B dense model, so the deploy path still has a size to plan with.
func fallbackShape() modelShape {
	return modelShape{
		Config: map[string]any{
			"architectures": []string{"LlamaForCausalLM"}, "model_type": "llama",
			"hidden_size": 4096, "num_hidden_layers": 32, "num_attention_heads": 32,
			"num_key_value_heads": 8, "intermediate_size": 14336, "vocab_size": 128256,
			"max_position_embeddings": 32768, "torch_dtype": "bfloat16",
		},
		WeightBytes: 15 << 30,
	}
}

// writeCheckpoint lays down a servable model directory whose weights are
// sparse files: the declared size is real to every size check, and the disk
// holds almost nothing. done reports the bytes written so far.
func writeCheckpoint(ctx context.Context, dir string, shape modelShape, pace time.Duration, done func(int64)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	config, err := json.MarshalIndent(shape.Config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), tokenizerJSON(), 0o644); err != nil {
		return err
	}
	shards := int((shape.WeightBytes + shardBytes - 1) / shardBytes)
	if shards < 1 {
		shards = 1
	}
	weightMap := map[string]string{}
	var written int64
	for i := 1; i <= shards; i++ {
		name := fmt.Sprintf("model-%05d-of-%05d.safetensors", i, shards)
		size := shardBytes
		if i == shards {
			size = shape.WeightBytes - shardBytes*int64(shards-1)
		}
		if err := sparseFile(filepath.Join(dir, name), size); err != nil {
			return err
		}
		weightMap[fmt.Sprintf("model.layers.%d.weight", i)] = name
		written += size
		if done != nil {
			done(written)
		}
		if pace > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pace):
			}
		}
	}
	index, err := json.MarshalIndent(map[string]any{
		"metadata":   map[string]any{"total_size": shape.WeightBytes},
		"weight_map": weightMap,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "model.safetensors.index.json"), index, 0o644)
}

// sparseFile creates a file of the given apparent size that occupies
// almost no disk. A non-empty first byte keeps "shard is empty" checks honest.
func sparseFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write([]byte{'{'}); err != nil {
		return err
	}
	return f.Truncate(size)
}
