package inferencecatalog

import (
	"encoding/json"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// FromFlatStoreModel reconstructs the user-facing catalog representation from
// its database projection. It is used when a stored model must become a
// portable package again, such as cloning an asset-bearing built-in template.
// The returned value is validated at the catalog/package boundary by callers.
func FromFlatStoreModel(row store.InferenceModel) (InferenceModel, error) {
	model := InferenceModel{
		ModelID: row.ModelID, Source: row.Source, DisplayTitle: row.DisplayTitle,
		ParameterCount: row.ParameterCount, Architecture: row.Architecture,
		ReadmeZHCN: row.ReadmeZHCN, ReadmeEN: row.ReadmeEN,
		ContextLength: row.ContextLength, ReleaseDate: row.ReleaseDate,
		Specs: make([]InferenceSpec, 0, len(row.Specs)),
	}
	if err := decodeProjectionJSON(row.ModelID, "input_modalities", row.InputModalities, &model.InputModalities); err != nil {
		return InferenceModel{}, err
	}
	if err := decodeProjectionJSON(row.ModelID, "kv_bytes_per_token", row.KVBytesPerToken, &model.KVBytesPerToken); err != nil {
		return InferenceModel{}, err
	}
	if err := decodeProjectionJSON(row.ModelID, "unsupported", row.Unsupported, &model.Unsupported); err != nil {
		return InferenceModel{}, err
	}
	if err := decodeProjectionJSON(row.ModelID, "model_config", row.ModelConfig, &model.ModelConfig); err != nil {
		return InferenceModel{}, err
	}
	for _, stored := range row.Specs {
		spec := InferenceSpec{
			Name: stored.Name, DisplayNameZHCN: stored.DisplayNameZHCN, DisplayNameEN: stored.DisplayNameEN, Scenario: stored.Scenario,
			Engine: stored.Engine, Image: stored.Image,
			Weights: Weights{Quantization: stored.Quantization}, WeightBytes: stored.WeightBytes, RuntimeWeightBytes: stored.RuntimeWeightBytes,
			PreStartScript: stored.PreStartScript, PDMode: stored.PDMode, ShmBytes: stored.ShmBytes, MemlockUnlimited: stored.MemlockUnlimited,
		}
		sources, err := DecodeWeightSources(stored.WeightSources)
		if err != nil {
			return InferenceModel{}, fmt.Errorf("decode stored inference catalog %s/%s weight_sources: %w", row.ModelID, stored.Name, err)
		}
		spec.Weights.Sources = sources
		imageSources, err := DecodeImageSources(stored.ImageSources)
		if err != nil {
			return InferenceModel{}, fmt.Errorf("decode stored inference catalog %s/%s image_sources: %w", row.ModelID, stored.Name, err)
		}
		spec.ImageSources = imageSources
		fields := []struct {
			name string
			raw  string
			out  any
		}{
			{"gpus", stored.GPUs, &spec.GPUs},
			{"args", stored.Args, &spec.Args},
			{"env", stored.Env, &spec.Env},
			{"pre_start_argv", stored.PreStartArgv, &spec.PreStartArgv},
			{"assets", stored.Assets, &spec.Assets},
			{"entrypoint", stored.Entrypoint, &spec.Entrypoint},
			{"placement", stored.Placement, &spec.Placement},
			{"api_contract", stored.APIContract, &spec.API},
			{"components", stored.Components, &spec.Components},
		}
		for _, field := range fields {
			if err := decodeProjectionJSON(row.ModelID+"/"+stored.Name, field.name, field.raw, field.out); err != nil {
				return InferenceModel{}, err
			}
		}
		model.Specs = append(model.Specs, spec)
	}
	return model, nil
}

func decodeProjectionJSON(where, field, raw string, output any) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), output); err != nil {
		return fmt.Errorf("decode stored inference catalog %s %s: %w", where, field, err)
	}
	return nil
}
