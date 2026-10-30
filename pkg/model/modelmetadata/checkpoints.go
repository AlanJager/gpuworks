package modelmetadata

import (
	"context"
	"encoding/json"
	"path"
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
	e, err := checkpointWithExtensions(ctx, in.Path, "mistral", files, []string{".safetensors"})
	if err != nil {
		return RuleResult{}, err
	}
	if len(e.Files) == 0 {
		return RuleResult{}, nil
	}
	e.Supersedes = []string{"transformers"}
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
	if len(e.Files) == 0 {
		return RuleResult{}, nil
	}
	e.Supersedes = []string{"transformers"}
	return RuleResult{Entries: []Entry{e}}, nil
}
