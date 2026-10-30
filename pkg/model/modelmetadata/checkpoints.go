package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func parseTransformers(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		Architectures []string `json:"architectures"`
		ModelType     string   `json:"model_type"`
		Class         string   `json:"_class_name"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "config.json"), &cfg)
	if err != nil || !ok {
		return RuleResult{}, err
	}
	// A Diffusers component's config is not a Transformers model contract.
	if cfg.Class != "" && len(cfg.Architectures) == 0 && cfg.ModelType == "" {
		return RuleResult{}, nil
	}
	if len(cfg.Architectures) == 0 && cfg.ModelType == "" {
		return RuleResult{}, nil
	}
	entry, err := checkpoint(ctx, in.Path, "transformers", files)
	if err != nil || len(entry.Files) == 0 {
		return RuleResult{}, err
	}
	return RuleResult{Entries: []Entry{entry}}, nil
}

// Checkpoint indices are authoritative: an unrelated weight file cannot mask
// a missing shard. Unindexed weights must be directly in this directory.
func checkpointWithNames(ctx context.Context, dir, format string, files FileView, names []string) (Entry, error) {
	e := Entry{Path: dir, Format: format}
	items, err := files.ReadDir(ctx, dir)
	if err != nil {
		return e, err
	}
	for _, name := range names {
		indexPath := path.Join(dir, name+".index.json")
		shards, ok, err := readCheckpointIndex(ctx, files, indexPath)
		if err != nil {
			return e, &fs.PathError{Op: "read checkpoint index", Path: indexPath, Err: err}
		}
		if !ok {
			continue
		}
		if len(shards) == 0 {
			return e, &fs.PathError{Op: "read checkpoint index", Path: indexPath, Err: errors.New("empty weight_map")}
		}
		for _, target := range shards {
			p, err := childPath(dir, target)
			if err != nil {
				return e, &fs.PathError{Op: "resolve checkpoint shard", Path: indexPath, Err: err}
			}
			if !slices.Contains(e.Files, p) {
				e.Files = append(e.Files, p)
			}
		}
		slices.Sort(e.Files)
		checkRequired(ctx, &e, files)
		return e, nil
	}
	for _, f := range items {
		if f.IsDir {
			continue
		}
		for _, name := range names {
			ext := path.Ext(name)
			if f.Name == name || (strings.HasPrefix(f.Name, strings.TrimSuffix(name, ext)+"-") && strings.HasSuffix(f.Name, ext)) {
				e.Files = append(e.Files, path.Join(dir, f.Name))
				break
			}
		}
	}
	// Some manually copied checkpoints have lost their index; the standard
	// shard names still provide explicit evidence of missing parts.
	for _, name := range slices.Clone(e.Files) {
		match := checkpointShard.FindStringSubmatch(path.Base(name))
		if match == nil {
			continue
		}
		count, _ := strconv.Atoi(match[2])
		if count > 10000 {
			return e, ErrReadLimit
		}
		for part := 1; part <= count; part++ {
			want := path.Join(dir, fmt.Sprintf("%s-%05d-of-%05d.%s", match[1], part, count, match[3]))
			if !slices.Contains(e.Files, want) {
				e.Files = append(e.Files, want)
			}
		}
		break
	}
	checkRequired(ctx, &e, files)
	return e, nil
}

var checkpointShard = regexp.MustCompile(`^(model|pytorch_model|diffusion_pytorch_model)-\d{5}-of-(\d{5})\.(safetensors|bin)$`)

func checkRequired(ctx context.Context, e *Entry, files FileView) {
	for _, name := range e.Files {
		if !exists(ctx, files, name) {
			e.Incomplete = true
			e.Missing = append(e.Missing, name)
		}
	}
}

func parseMistral(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var params map[string]json.RawMessage
	ok, err := readJSON(ctx, files, path.Join(in.Path, "params.json"), &params)
	if err != nil || !ok || len(params) == 0 {
		return RuleResult{}, err
	}
	if !exists(ctx, files, path.Join(in.Path, "tekken.json")) {
		return RuleResult{}, nil
	}
	items, err := files.ReadDir(ctx, in.Path)
	if err != nil {
		return RuleResult{}, err
	}
	e := Entry{Path: in.Path, Format: "mistral", Supersedes: []string{"transformers"}}
	for _, item := range items {
		if !item.IsDir && strings.HasPrefix(item.Name, "consolidated") && strings.HasSuffix(item.Name, ".safetensors") {
			e.Files = append(e.Files, path.Join(in.Path, item.Name))
		}
	}
	if len(e.Files) == 0 {
		return RuleResult{}, nil
	}
	checkRequired(ctx, &e, files)
	return RuleResult{Entries: []Entry{e}}, nil
}

func parseAdapter(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		PEFTType string `json:"peft_type"`
		Base     string `json:"base_model_name_or_path"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "adapter_config.json"), &cfg)
	if err != nil || !ok || cfg.PEFTType == "" {
		return RuleResult{}, err
	}
	return RuleResult{Components: []Reference{{Path: in.Path, Role: "adapter", Repository: cfg.Base, Source: "adapter_config.json"}}}, nil
}

func parseModelScope(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		Task  string                     `json:"task"`
		Model map[string]json.RawMessage `json:"model"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "configuration.json"), &cfg)
	if err != nil || !ok || cfg.Task == "" || len(cfg.Model) == 0 {
		return RuleResult{}, err
	}
	e, err := checkpoint(ctx, in.Path, "modelscope", files)
	if err != nil {
		return RuleResult{}, err
	}
	if len(e.Files) == 0 && exists(ctx, files, path.Join(in.Path, "pytorch_model.pt")) {
		e.Files = []string{path.Join(in.Path, "pytorch_model.pt")}
	}
	if len(e.Files) == 0 {
		return RuleResult{}, nil
	}
	e.Supersedes = []string{"transformers"}
	return RuleResult{Entries: []Entry{e}}, nil
}
