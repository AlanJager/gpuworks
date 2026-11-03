package inferencecatalog

import (
	"encoding/json"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// ToFlatStoreModels converts the validated, user-facing representation to the
// two persistence rows. The nested YAML shape is intentionally flattened only
// at this boundary; no legacy template graph is produced.
func ToFlatStoreModels(catalog FlatCatalog) ([]store.InferenceModel, error) {
	if err := ValidateFlatCatalog(&catalog); err != nil {
		return nil, err
	}
	models := make([]store.InferenceModel, 0, len(catalog.Models))
	for _, model := range catalog.Models {
		unsupported, err := json.Marshal(model.Unsupported)
		if err != nil {
			return nil, fmt.Errorf("model %s unsupported: %w", model.ModelID, err)
		}
		modalities, err := json.Marshal(model.InputModalities)
		if err != nil {
			return nil, fmt.Errorf("model %s input_modalities: %w", model.ModelID, err)
		}
		source := model.Source
		if source == "" {
			source = "upstream"
		}
		modelConfig := ""
		if model.ModelConfig != nil {
			raw, err := json.Marshal(model.ModelConfig)
			if err != nil {
				return nil, fmt.Errorf("model %s model_config: %w", model.ModelID, err)
			}
			modelConfig = string(raw)
		}
		kvBytes, err := json.Marshal(model.KVBytesPerToken)
		if err != nil {
			return nil, fmt.Errorf("model %s kv_bytes_per_token: %w", model.ModelID, err)
		}
		row := store.InferenceModel{
			ModelID: model.ModelID, DisplayTitle: model.DisplayTitle,
			ParameterCount: model.ParameterCount, Architecture: model.Architecture, InputModalities: string(modalities),
			ModelConfig: modelConfig,
			ReadmeZHCN:  model.ReadmeZHCN, ReadmeEN: model.ReadmeEN,
			ContextLength: model.ContextLength, ReleaseDate: model.ReleaseDate,
			KVBytesPerToken: string(kvBytes),
			Source:          source, Unsupported: string(unsupported),
			Specs: make([]store.InferenceSpecRow, 0, len(model.Specs)),
		}
		for _, spec := range model.Specs {
			weights, err := json.Marshal(spec.Weights.Sources)
			if err != nil {
				return nil, fmt.Errorf("model %s spec %s weights: %w", model.ModelID, spec.Name, err)
			}
			gpus, err := json.Marshal(spec.GPUs)
			if err != nil {
				return nil, fmt.Errorf("model %s spec %s gpus: %w", model.ModelID, spec.Name, err)
			}
			args, _ := json.Marshal(spec.Args)
			env, _ := json.Marshal(spec.Env)
			preStartArgv, _ := json.Marshal(spec.PreStartArgv)
			assets, _ := json.Marshal(spec.Assets)
			entrypoint, _ := json.Marshal(spec.Entrypoint)
			placement, _ := json.Marshal(spec.Placement)
			apiContract, _ := json.Marshal(spec.API)
			components, _ := json.Marshal(spec.Components)
			imageSources, err := EncodeImageSources(spec.ImageSources)
			if err != nil {
				return nil, fmt.Errorf("model %s spec %s image_sources: %w", model.ModelID, spec.Name, err)
			}
			row.Specs = append(row.Specs, store.InferenceSpecRow{
				Source: source, Name: spec.Name, DisplayNameZHCN: spec.DisplayNameZHCN, DisplayNameEN: spec.DisplayNameEN, Scenario: spec.Scenario, Engine: spec.Engine,
				Image: spec.Image, ImageSources: imageSources, Quantization: spec.Weights.Quantization, WeightBytes: spec.WeightBytes, RuntimeWeightBytes: spec.RuntimeWeightBytes,
				WeightSources: string(weights), GPUs: string(gpus), Args: string(args),
				Env: string(env), PreStartArgv: string(preStartArgv), PreStartScript: spec.PreStartScript,
				Assets: string(assets), Entrypoint: string(entrypoint), Placement: string(placement),
				APIContract: string(apiContract), Components: string(components), PDMode: spec.PDMode, ShmBytes: spec.ShmBytes, MemlockUnlimited: spec.MemlockUnlimited,
			})
		}
		models = append(models, row)
	}
	return models, nil
}

// ReconcileUpstreamCatalog applies a validated package projection while keeping
// user-created models and specs. Embedded data and imported data share this
// existing persistence boundary; the source package does not need a Store.
func ReconcileUpstreamCatalog(st *store.Store, catalog FlatCatalog, digest string) error {
	if st == nil {
		return fmt.Errorf("seed inference catalog: store is nil")
	}
	models, err := ToFlatStoreModels(catalog)
	if err != nil {
		return fmt.Errorf("convert embedded inference catalog: %w", err)
	}
	for index := range models {
		models[index].PackageDigest = digest
		for specIndex := range models[index].Specs {
			models[index].Specs[specIndex].Source = "upstream"
			models[index].Specs[specIndex].PackageDigest = digest
		}
	}
	if err := st.ReconcileInferenceCatalog(models); err != nil {
		return fmt.Errorf("reconcile embedded inference catalog: %w", err)
	}
	return nil
}
