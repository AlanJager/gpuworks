package experience

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// checkpoint is the file layout of shape's published checkpoint: config,
// tokenizer, weight shards of shardBytes and their index. The offline hub
// serves it and the preloaded models are written from it, so a model reads
// the same whichever way it arrived.
func checkpoint(shape modelShape) []repoFile {
	config, _ := json.MarshalIndent(shape.Config, "", "  ")
	shards := max(int((shape.WeightBytes+shardBytes-1)/shardBytes), 1)
	weightMap := map[string]string{}
	out := []repoFile{{Path: "config.json", Data: config}, {Path: "tokenizer.json", Data: tokenizerJSON()}}
	for i := 1; i <= shards; i++ {
		name := fmt.Sprintf("model-%05d-of-%05d.safetensors", i, shards)
		size := shardBytes
		if i == shards {
			size = shape.WeightBytes - shardBytes*int64(shards-1)
		}
		weightMap[fmt.Sprintf("model.layers.%d.weight", i)] = name
		out = append(out, repoFile{Path: name, Size: size})
	}
	index, _ := json.MarshalIndent(map[string]any{"metadata": map[string]any{"total_size": shape.WeightBytes}, "weight_map": weightMap}, "", "  ")
	return append(out, repoFile{Path: "model.safetensors.index.json", Data: index})
}

// writeCheckpoint lays down shape's checkpoint in dir with sparse weights:
// the declared size is real to every size check, and the disk holds almost
// nothing.
func writeCheckpoint(dir string, shape modelShape) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range checkpoint(shape) {
		path := filepath.Join(dir, f.Path)
		var err error
		if f.Data != nil {
			err = os.WriteFile(path, f.Data, 0o644)
		} else {
			err = sparseFile(path, f.Size)
		}
		if err != nil {
			return err
		}
	}
	return nil
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
