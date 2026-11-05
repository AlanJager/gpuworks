package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path"
	"strings"
)

// Component contracts describe saved files, not pipeline roles: a scheduler
// needs configuration while a model needs weights. Unknown Python classes
// cannot be verified by the presence of their directory alone.
type componentContract struct {
	config  string
	weights []string
	// Any one complete set satisfies non-weight assets (e.g. fast/slow tokenizers).
	fileSets [][]string
}

func sentenceTransformerContract(class string) (componentContract, bool) {
	switch class {
	case "Normalize":
		return componentContract{}, true // save() writes no files.
	case "Transformer", "Dense":
		return componentContract{config: "config.json", weights: []string{"model.safetensors", "pytorch_model.bin"}}, true
	case "Pooling", "LogitScore":
		return componentContract{config: "config.json"}, true
	default:
		return componentContract{}, false
	}
}

func diffusersComponentContract(library, class string) (componentContract, bool) {
	if library == "transformers" {
		switch class {
		case "CLIPTokenizer":
			return componentContract{fileSets: [][]string{{"vocab.json", "merges.txt"}}}, true
		case "CLIPTokenizerFast":
			return componentContract{fileSets: [][]string{{"tokenizer.json"}, {"vocab.json", "merges.txt"}}}, true
		case "T5Tokenizer":
			return componentContract{fileSets: [][]string{{"spiece.model"}}}, true
		case "T5TokenizerFast":
			return componentContract{fileSets: [][]string{{"tokenizer.json"}, {"spiece.model"}}}, true
		case "CLIPImageProcessor", "CLIPFeatureExtractor":
			return componentContract{config: "preprocessor_config.json"}, true
		}
	}
	switch {
	case library == "diffusers" && strings.HasSuffix(class, "Scheduler"):
		return componentContract{config: "scheduler_config.json"}, true
	case library == "diffusers" && (strings.Contains(class, "Model") || strings.HasPrefix(class, "Autoencoder") || strings.HasPrefix(class, "UNet")):
		return componentContract{config: "config.json", weights: []string{"diffusion_pytorch_model.safetensors", "diffusion_pytorch_model.bin"}}, true
	case library == "transformers" && strings.Contains(class, "Model"):
		return componentContract{config: "config.json", weights: []string{"model.safetensors", "pytorch_model.bin"}}, true
	default:
		return componentContract{}, false
	}
}

func validateComponent(ctx context.Context, files FileView, dir, class string, contract componentContract, known bool, entry *Entry, result *RuleResult) error {
	if !known {
		entry.Incomplete = true
		result.Issues = append(result.Issues, Issue{Path: dir, Code: "unverified_component", Detail: "no local validation contract is available for component class " + class})
		return nil
	}
	if contract.config == "" && len(contract.weights) == 0 && len(contract.fileSets) == 0 {
		return nil
	}
	info, err := files.Stat(ctx, dir)
	if err != nil {
		return componentReadError(dir, err, entry, result)
	}
	if !info.IsDir {
		return componentReadError(dir, fs.ErrInvalid, entry, result)
	}
	if contract.config != "" {
		name := path.Join(dir, contract.config)
		var config map[string]json.RawMessage
		ok, err := readJSON(ctx, files, name, &config)
		if err != nil {
			if err := componentReadError(name, err, entry, result); err != nil {
				return err
			}
		} else if !ok {
			entry.Incomplete = true
			entry.Missing = append(entry.Missing, name)
		}
	}
	if len(contract.fileSets) > 0 {
		if err := validateComponentFileSets(ctx, files, dir, contract.fileSets, entry, result); err != nil {
			return err
		}
	}
	if len(contract.weights) == 0 {
		return nil
	}
	weights, issues, err := checkpointWithNames(ctx, dir, entry.Format, files, contract.weights)
	if err != nil {
		return componentReadError(dir, err, entry, result)
	}
	entry.Files = append(entry.Files, weights.Files...)
	result.Issues = append(result.Issues, issues...)
	entry.Incomplete = entry.Incomplete || weights.Incomplete
	entry.Missing = append(entry.Missing, weights.Missing...)
	if len(weights.Files) == 0 {
		entry.Incomplete = true
		entry.Missing = append(entry.Missing, path.Join(dir, "model weights"))
	}
	return nil
}

func validateComponentFileSets(ctx context.Context, files FileView, dir string, sets [][]string, entry *Entry, result *RuleResult) error {
	var alternatives []string
	for _, set := range sets {
		var missing []string
		for _, name := range set {
			name = path.Join(dir, name)
			info, err := files.Stat(ctx, name)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return componentReadError(name, err, entry, result)
			}
			if err != nil || info.IsDir || info.Size == 0 {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return nil
		}
		alternatives = append(alternatives, strings.Join(missing, " + "))
	}
	entry.Incomplete = true
	entry.Missing = append(entry.Missing, strings.Join(alternatives, " or "))
	return nil
}

func componentReadError(name string, err error, entry *Entry, result *RuleResult) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	entry.Incomplete = true
	if errors.Is(err, fs.ErrNotExist) {
		entry.Missing = append(entry.Missing, name)
	} else {
		result.Issues = append(result.Issues, issue(name, "metadata_unreadable", err))
	}
	return nil
}
