package modelmetadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
	if err != nil || !ok {
		return RuleResult{}, err
	}
	if len(modules) == 0 {
		return unrecognized(path.Join(in.Path, "modules.json"), "Sentence Transformers manifest declares no modules"), nil
	}
	e := Entry{Path: in.Path, Format: "sentence-transformers", Independent: true, Supersedes: []string{"transformers", "modelscope"}}
	r := RuleResult{Repository: true}
	for _, module := range modules {
		if module.Type == "" {
			return RuleResult{}, metadataError(path.Join(in.Path, "modules.json"), errors.New("module has no type"))
		}
		dir := in.Path
		if module.Path != "" && module.Path != "." {
			dir, err = childPath(in.Path, module.Path)
			if err != nil {
				return RuleResult{}, metadataError(path.Join(in.Path, "modules.json"), fmt.Errorf("module path %q: %w", module.Path, err))
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

var diffusersManifestNames = []string{"modular_model_index.json", "model_index.json"}

func parseDiffusers(ctx context.Context, in Input, files FileView) (RuleResult, error) {
	for index, name := range diffusersManifestNames {
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
			return RuleResult{}, metadataError(path.Join(in.Path, name), errors.New("missing _class_name"))
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
				return RuleResult{}, metadataError(path.Join(in.Path, name), fmt.Errorf("%s: component library and class must be non-empty strings", key))
			}
			ref := Reference{Role: "component", Source: name}
			if len(spec) > 2 {
				var location struct {
					Repo      string `json:"repo"`
					Subfolder string `json:"subfolder"`
				}
				if err := json.Unmarshal(spec[2], &location); err != nil {
					return RuleResult{}, metadataError(path.Join(in.Path, name), fmt.Errorf("%s: %w", key, err))
				}
				ref.Repository, ref.Subfolder = location.Repo, location.Subfolder
			}
			// Explicit remote dependencies cannot be satisfied by a similarly
			// named local directory. Their ownership is kept for the caller.
			if ref.Repository != "" {
				e.Incomplete = true
				e.Missing = append(e.Missing, ref.Repository+"/"+ref.Subfolder)
				r.Issues = append(r.Issues, Issue{Path: in.Path, Code: "external_component", Detail: "component requires an external repository: " + ref.Repository + "/" + ref.Subfolder})
			} else {
				ref.Path, err = childPath(in.Path, key)
				if err != nil {
					return RuleResult{}, metadataError(path.Join(in.Path, name), fmt.Errorf("component path %q: %w", key, err))
				}
				contract, known := diffusersComponentContract(library, componentClass)
				if err := validateComponent(ctx, files, ref.Path, library+"."+componentClass, contract, known, &e, &r); err != nil {
					return RuleResult{}, err
				}
			}
			r.Components = append(r.Components, ref)
		}
		if len(r.Components) == 0 {
			return RuleResult{}, metadataError(path.Join(in.Path, name), errors.New("pipeline manifest declares no components"))
		}
		components, issues, err := readAdditionalDiffusersComponents(ctx, in, files, diffusersManifestNames[index+1:])
		if err != nil {
			return RuleResult{}, err
		}
		for _, ref := range components {
			if !slices.ContainsFunc(r.Components, func(existing Reference) bool { return existing.Path == ref.Path }) {
				r.Components = append(r.Components, ref)
			}
		}
		r.Issues = append(r.Issues, issues...)
		r.Entries = []Entry{e}
		return r, nil
	}
	return RuleResult{}, nil
}

// Other pipeline manifests can own local components the selected entry does
// not use. Their declarations do not add requirements to that entry.
func readAdditionalDiffusersComponents(ctx context.Context, in Input, files FileView, names []string) ([]Reference, []Issue, error) {
	var refs []Reference
	var issues []Issue
	for _, name := range names {
		manifestPath := path.Join(in.Path, name)
		var manifest map[string]json.RawMessage
		ok, err := readJSON(ctx, files, manifestPath, &manifest)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		if err != nil {
			issues = append(issues, issue(manifestPath, "metadata_unreadable", err))
			continue
		}
		if !ok {
			continue
		}
		var class string
		if json.Unmarshal(manifest["_class_name"], &class) != nil || class == "" {
			issues = append(issues, issue(manifestPath, "invalid_metadata", errors.New("missing _class_name")))
			continue
		}
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
			}
			if string(spec[0]) == "null" || string(spec[1]) == "null" {
				continue
			}
			var library, componentClass string
			if json.Unmarshal(spec[0], &library) != nil || json.Unmarshal(spec[1], &componentClass) != nil || library == "" || componentClass == "" {
				issues = append(issues, issue(manifestPath, "invalid_metadata", errors.New(key+": component library and class must be non-empty strings")))
				continue
			}
			localPath := key
			if len(spec) > 2 {
				var location struct {
					Repo       string `json:"repo"`
					Pretrained string `json:"pretrained_model_name_or_path"`
					Subfolder  string `json:"subfolder"`
				}
				if err := json.Unmarshal(spec[2], &location); err != nil {
					issues = append(issues, issue(manifestPath, "invalid_metadata", err))
					continue
				}
				if location.Repo != "" || location.Pretrained != "" {
					continue
				}
				if location.Subfolder != "" {
					localPath = location.Subfolder
				}
			}
			componentPath, err := childPath(in.Path, localPath)
			if err != nil || componentPath == in.Path || (in.Path != "." && !strings.HasPrefix(componentPath, in.Path+"/")) {
				issues = append(issues, issue(manifestPath, "invalid_metadata", errors.New(key+": component path must stay within the model directory")))
				continue
			}
			refs = append(refs, Reference{Path: componentPath, Role: "component", Source: name})
		}
	}
	return refs, issues, nil
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
	items, err := files.ReadDir(ctx, in.Path)
	if err != nil {
		return RuleResult{}, err
	}
	r := RuleResult{}
	onnxPath := path.Join(in.Path, "model.onnx")
	onnxErr := requiredFileError(ctx, files, onnxPath)
	if errors.Is(onnxErr, context.Canceled) || errors.Is(onnxErr, context.DeadlineExceeded) {
		return RuleResult{}, onnxErr
	}
	if hasName(items, "model.onnx") || onnxErr == nil {
		if ok && (cfg.ModelType != "" || len(cfg.Architectures) > 0) && onnxErr == nil {
			return RuleResult{Entries: []Entry{{Path: in.Path, Format: "onnx", Files: []string{path.Join(in.Path, "model.onnx")}, Supersedes: []string{"transformers"}}}}, nil
		}
		if !ok || (cfg.ModelType == "" && len(cfg.Architectures) == 0) {
			r = unrecognized(path.Join(in.Path, "config.json"), "ONNX model requires a configuration declaring model_type or architectures")
		}
		if onnxErr != nil {
			r.Issues = append(r.Issues, issue(onnxPath, unrecognizedModelIssue, onnxErr))
		}
	}
	graphErr := requiredFileError(ctx, files, path.Join(in.Path, "saved_model.pb"))
	variablesErr := requiredFileError(ctx, files, path.Join(in.Path, "variables/variables.index"))
	for _, err := range []error{graphErr, variablesErr} {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return RuleResult{}, err
		}
	}
	if graphErr == nil && variablesErr == nil {
		return RuleResult{Entries: []Entry{{Path: in.Path, Format: "savedmodel", Files: []string{path.Join(in.Path, "saved_model.pb"), path.Join(in.Path, "variables/variables.index")}}}}, nil
	}
	// File presence identifies the layout even when none of its assets is valid.
	// A generic variables directory alone is not evidence of a SavedModel.
	if hasName(items, "saved_model.pb") || graphErr == nil || !errors.Is(variablesErr, fs.ErrNotExist) {
		for _, failure := range []struct {
			name string
			err  error
		}{{"saved_model.pb", graphErr}, {"variables/variables.index", variablesErr}} {
			if failure.err != nil {
				r.Issues = append(r.Issues, issue(path.Join(in.Path, failure.name), unrecognizedModelIssue, failure.err))
			}
		}
	}
	return r, nil
}
