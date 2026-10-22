package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
)

func parseSentenceTransformers(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var modules []struct {
		Path string `json:"path"`
		Type string `json:"type"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "modules.json"), &modules)
	if err != nil || !ok || len(modules) == 0 {
		return RuleResult{}, err
	}
	e := Entry{Path: in.Path, Format: "sentence-transformers", Independent: true, Supersedes: []string{"transformers", "modelscope"}}
	r := RuleResult{Repository: true}
	for _, module := range modules {
		if module.Type == "" {
			return RuleResult{}, errors.New("modules.json: module has no type")
		}
		dir := in.Path
		if module.Path != "" && module.Path != "." {
			dir, err = childPath(in.Path, module.Path)
			if err != nil {
				return RuleResult{}, err
			}
			r.Components = append(r.Components, Reference{Path: dir, Role: "component", Source: "modules.json"})
		}
		contract, known := sentenceTransformerContract(sentenceTransformerModuleClass(module.Type))
		if err := validateComponent(ctx, files, dir, module.Type, contract, known, &e, &r); err != nil {
			return RuleResult{}, err
		}
	}
	r.Entries = []Entry{e}
	return r, nil
}

// Both legacy and newer Sentence Transformers manifests use fully qualified
// Python types; module package paths changed while the class contracts did not.
func sentenceTransformerModuleClass(name string) string {
	if !strings.HasPrefix(name, "sentence_transformers.") {
		return ""
	}
	return name[strings.LastIndexByte(name, '.')+1:]
}

func parseDiffusers(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	for _, name := range []string{"modular_model_index.json", "model_index.json"} {
		var manifest map[string]json.RawMessage
		ok, err := readJSON(ctx, files, path.Join(in.Path, name), &manifest)
		if err != nil {
			return RuleResult{}, err
		}
		if !ok {
			continue
		}
		var class string
		if json.Unmarshal(manifest["_class_name"], &class) != nil || class == "" {
			return RuleResult{}, errors.New(name + ": missing _class_name")
		}
		format := "diffusers"
		if name == "modular_model_index.json" {
			format = "modular-diffusers"
		}
		e := Entry{Path: in.Path, Format: format, Independent: true, Supersedes: []string{"transformers", "modelscope"}}
		r := RuleResult{Repository: true}
		keys := make([]string, 0, len(manifest))
		for key := range manifest {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			if strings.HasPrefix(key, "_") {
				continue
			}
			var spec []json.RawMessage
			if json.Unmarshal(manifest[key], &spec) != nil || len(spec) < 2 {
				continue
			} // scalar pipeline settings
			if string(spec[0]) == "null" || string(spec[1]) == "null" {
				continue
			} // optional absent component
			var library, componentClass string
			if json.Unmarshal(spec[0], &library) != nil || json.Unmarshal(spec[1], &componentClass) != nil || library == "" || componentClass == "" {
				return RuleResult{}, errors.New(name + ": component library and class must be non-empty strings")
			}
			ref := Reference{Role: "component", Source: name}
			if len(spec) > 2 {
				var location struct {
					Repo      string `json:"repo"`
					Subfolder string `json:"subfolder"`
				}
				if err := json.Unmarshal(spec[2], &location); err != nil {
					return RuleResult{}, err
				}
				ref.Repository, ref.Subfolder = location.Repo, location.Subfolder
			}
			// Explicit remote dependencies cannot be satisfied by a similarly
			// named local directory. Their ownership is kept for the caller.
			if ref.Repository != "" {
				e.Incomplete = true
				e.Missing = append(e.Missing, ref.Repository+"/"+ref.Subfolder)
				r.Issues = append(r.Issues, Issue{Path: in.Path, Code: "external_component", Detail: ref.Repository + "/" + ref.Subfolder})
			} else {
				ref.Path, err = childPath(in.Path, key)
				if err != nil {
					return RuleResult{}, err
				}
				contract, known := diffusersComponentContract(library, componentClass)
				if err := validateComponent(ctx, files, ref.Path, library+"."+componentClass, contract, known, &e, &r); err != nil {
					return RuleResult{}, err
				}
			}
			r.Components = append(r.Components, ref)
		}
		if len(r.Components) == 0 {
			return RuleResult{}, errors.New(name + ": no components")
		}
		r.Entries = []Entry{e}
		return r, nil
	}
	return RuleResult{}, nil
}

// Export layouts need their surrounding contract. A random .onnx/.pb file is
// not enough to turn a component directory into an independent model.
func parseExports(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	var cfg struct {
		ModelType     string   `json:"model_type"`
		Architectures []string `json:"architectures"`
	}
	ok, err := readJSON(ctx, files, path.Join(in.Path, "config.json"), &cfg)
	if err != nil {
		return RuleResult{}, err
	}
	if ok && (cfg.ModelType != "" || len(cfg.Architectures) > 0) {
		if exists(ctx, files, path.Join(in.Path, "model.onnx")) {
			return RuleResult{Entries: []Entry{{Path: in.Path, Format: "onnx", Files: []string{path.Join(in.Path, "model.onnx")}, Supersedes: []string{"transformers"}}}}, nil
		}
	}
	if exists(ctx, files, path.Join(in.Path, "saved_model.pb")) && exists(ctx, files, path.Join(in.Path, "variables/variables.index")) {
		return RuleResult{Entries: []Entry{{Path: in.Path, Format: "savedmodel", Files: []string{path.Join(in.Path, "saved_model.pb"), path.Join(in.Path, "variables/variables.index")}}}}, nil
	}
	return RuleResult{}, nil
}
