package api

import (
	"errors"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/inferencecatalog"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A template may mount another catalog model as a component (a speculative
// draft such as incoai/glm-5.3-flash-dflash2). Catalog validation resolves
// that reference inside one document or package, but a document built from
// or for a single template - a clone, the template editor's YAML - holds only
// that template. These helpers let such documents resolve the reference
// against the models the server already has, without writing those rows.

// withComponentModels appends the models that the catalog's specs mount as
// components but the catalog does not carry, read from the server. The
// entries are component-only - no specs, no documentation - so a package
// built with them gains no files, and they mirror rows nobody rewrites. A
// component model the server does not know stays missing, and validation
// reports it.
func (a *apiHandlers) withComponentModels(catalog inferencecatalog.FlatCatalog) (inferencecatalog.FlatCatalog, error) {
	// Identities are compared as validation normalizes them, so an unvalidated
	// YAML document can be augmented before it is checked.
	normalize := func(modelID string) string { return strings.ToLower(strings.TrimSpace(modelID)) }
	carried := make(map[string]struct{}, len(catalog.Models))
	for _, model := range catalog.Models {
		carried[normalize(model.ModelID)] = struct{}{}
	}
	models := append([]inferencecatalog.InferenceModel(nil), catalog.Models...)
	for _, model := range catalog.Models {
		for _, spec := range model.Specs {
			for _, component := range spec.Components {
				componentID := normalize(component.ModelID)
				if componentID == "" {
					continue
				}
				if _, ok := carried[componentID]; ok {
					continue
				}
				carried[componentID] = struct{}{}
				row, err := a.d.Store.InferenceModelByModelID(componentID)
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				if err != nil {
					return inferencecatalog.FlatCatalog{}, err
				}
				referenced := *row
				referenced.Specs = nil
				entry, err := inferencecatalog.FromFlatStoreModel(referenced)
				if err != nil {
					return inferencecatalog.FlatCatalog{}, err
				}
				entry.ReadmeZHCN, entry.ReadmeEN = "", ""
				models = append(models, entry)
			}
		}
	}
	catalog.Models = models
	return catalog, nil
}

// validateWithComponentModels validates a decoded YAML document together with
// the component models the server already has; the document keeps only its
// own models.
func (a *apiHandlers) validateWithComponentModels(catalog *inferencecatalog.FlatCatalog) error {
	augmented, err := a.withComponentModels(*catalog)
	if err != nil {
		return err
	}
	if err := inferencecatalog.ValidateFlatCatalog(&augmented); err != nil {
		return err
	}
	catalog.Models = augmented.Models[:len(catalog.Models)]
	return nil
}

// importedStoreModels converts an imported catalog into rows, validated with
// the component models the server already has (withComponentModels); the rows
// are the catalog's own models only.
func (a *apiHandlers) importedStoreModels(catalog inferencecatalog.FlatCatalog) ([]store.InferenceModel, error) {
	augmented, err := a.withComponentModels(catalog)
	if err != nil {
		return nil, err
	}
	models, err := inferencecatalog.ToFlatStoreModels(augmented)
	if err != nil {
		return nil, err
	}
	return models[:len(catalog.Models)], nil
}

// modelsNamedIn keeps the models of from that names also carries.
func modelsNamedIn(from, names inferencecatalog.FlatCatalog) inferencecatalog.FlatCatalog {
	wanted := make(map[string]struct{}, len(names.Models))
	for _, model := range names.Models {
		wanted[strings.ToLower(strings.TrimSpace(model.ModelID))] = struct{}{}
	}
	var kept inferencecatalog.FlatCatalog
	for _, model := range from.Models {
		if _, ok := wanted[strings.ToLower(strings.TrimSpace(model.ModelID))]; ok {
			kept.Models = append(kept.Models, model)
		}
	}
	return kept
}
