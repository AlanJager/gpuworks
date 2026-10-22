// Package templateprobe implements the internal L1/L2/L3 validation primitives
// used by the inference-template catalog. Probe success is never compatibility
// proof; only deterministic, classified failures may exclude a combination.
package templateprobe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ModelConfig is the config.json subset needed by static inspection and the
// MoE-aware truncation policy. Unknown fields remain available in Raw.
type ModelConfig struct {
	Architectures        []string        `json:"architectures,omitempty"`
	ModelType            string          `json:"model_type,omitempty"`
	NumHiddenLayers      int             `json:"num_hidden_layers,omitempty"`
	MaxPositionEmbedding int64           `json:"max_position_embeddings,omitempty"`
	FirstKDenseReplace   *int            `json:"first_k_dense_replace,omitempty"`
	MoELayerFreq         *int            `json:"moe_layer_freq,omitempty"`
	DecoderSparseStep    *int            `json:"decoder_sparse_step,omitempty"`
	MLPOnlyLayers        []int           `json:"mlp_only_layers,omitempty"`
	NumExperts           *int            `json:"num_experts,omitempty"`
	NRoutedExperts       *int            `json:"n_routed_experts,omitempty"`
	NumLocalExperts      *int            `json:"num_local_experts,omitempty"`
	LayerTypes           []string        `json:"layer_types,omitempty"`
	ParameterCount       string          `json:"parameter_count,omitempty"`
	TorchDtype           string          `json:"torch_dtype,omitempty"`
	QuantizationMethod   string          `json:"quantization_method,omitempty"`
	Multimodal           bool            `json:"multimodal,omitempty"`
	Raw                  json.RawMessage `json:"-"`
}

type rawModelConfig struct {
	Architectures        []string        `json:"architectures"`
	ModelType            string          `json:"model_type"`
	NumHiddenLayers      int             `json:"num_hidden_layers"`
	MaxPositionEmbedding int64           `json:"max_position_embeddings"`
	FirstKDenseReplace   *int            `json:"first_k_dense_replace"`
	MoELayerFreq         *int            `json:"moe_layer_freq"`
	DecoderSparseStep    *int            `json:"decoder_sparse_step"`
	MLPOnlyLayers        []int           `json:"mlp_only_layers"`
	NumExperts           *int            `json:"num_experts"`
	NRoutedExperts       *int            `json:"n_routed_experts"`
	NumLocalExperts      *int            `json:"num_local_experts"`
	LayerTypes           []string        `json:"layer_types"`
	TorchDtype           string          `json:"torch_dtype"`
	NumParameters        json.RawMessage `json:"num_parameters"`
	ParameterCount       json.RawMessage `json:"parameter_count"`
	QuantizationConfig   map[string]any  `json:"quantization_config"`
	TextConfig           json.RawMessage `json:"text_config"`
	ThinkerConfig        json.RawMessage `json:"thinker_config"`
	VisionConfig         json.RawMessage `json:"vision_config"`
	AudioConfig          json.RawMessage `json:"audio_config"`
}

// ParseModelConfig promotes the effective language config from text_config or
// thinker_config while preserving top-level architecture and modality facts.
func ParseModelConfig(raw []byte) (ModelConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var outer rawModelConfig
	if err := dec.Decode(&outer); err != nil {
		return ModelConfig{}, fmt.Errorf("parse config.json: %w", err)
	}
	effective := outer
	multimodal := presentJSON(outer.VisionConfig) || presentJSON(outer.AudioConfig)
	if nested, ok := decodeRawConfig(outer.ThinkerConfig); ok {
		multimodal = multimodal || presentJSON(nested.VisionConfig) || presentJSON(nested.AudioConfig)
		effective = mergeRawConfig(effective, nested)
		if len(nested.TextConfig) > 0 {
			if text, valid := decodeRawConfig(nested.TextConfig); valid {
				effective = mergeRawConfig(effective, text)
			}
		}
	}
	if text, ok := decodeRawConfig(outer.TextConfig); ok {
		effective = mergeRawConfig(effective, text)
	}
	config := ModelConfig{
		Architectures:        append([]string(nil), outer.Architectures...),
		ModelType:            outer.ModelType,
		NumHiddenLayers:      effective.NumHiddenLayers,
		MaxPositionEmbedding: effective.MaxPositionEmbedding,
		FirstKDenseReplace:   cloneInt(effective.FirstKDenseReplace),
		MoELayerFreq:         cloneInt(effective.MoELayerFreq),
		DecoderSparseStep:    cloneInt(effective.DecoderSparseStep),
		MLPOnlyLayers:        append([]int(nil), effective.MLPOnlyLayers...),
		NumExperts:           cloneInt(effective.NumExperts),
		NRoutedExperts:       cloneInt(effective.NRoutedExperts),
		NumLocalExperts:      cloneInt(effective.NumLocalExperts),
		LayerTypes:           append([]string(nil), effective.LayerTypes...),
		TorchDtype:           firstNonempty(outer.TorchDtype, effective.TorchDtype),
		ParameterCount:       parameterCount(outer, effective),
		QuantizationMethod:   quantizationMethod(outer, effective),
		Multimodal:           multimodal,
		Raw:                  append(json.RawMessage(nil), raw...),
	}
	if len(config.Architectures) == 0 {
		config.Architectures = append([]string(nil), effective.Architectures...)
	}
	if config.ModelType == "" {
		config.ModelType = effective.ModelType
	}
	return config, nil
}

func decodeRawConfig(raw json.RawMessage) (rawModelConfig, bool) {
	if !presentJSON(raw) {
		return rawModelConfig{}, false
	}
	var config rawModelConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&config) != nil {
		return rawModelConfig{}, false
	}
	return config, true
}

func mergeRawConfig(base, nested rawModelConfig) rawModelConfig {
	// Nested language-model fields are authoritative when present. The caller
	// retains top-level architecture/model type separately for display.
	if nested.NumHiddenLayers > 0 {
		base.NumHiddenLayers = nested.NumHiddenLayers
	}
	if nested.MaxPositionEmbedding > 0 {
		base.MaxPositionEmbedding = nested.MaxPositionEmbedding
	}
	if nested.FirstKDenseReplace != nil {
		base.FirstKDenseReplace = nested.FirstKDenseReplace
	}
	if nested.MoELayerFreq != nil {
		base.MoELayerFreq = nested.MoELayerFreq
	}
	if nested.DecoderSparseStep != nil {
		base.DecoderSparseStep = nested.DecoderSparseStep
	}
	if nested.MLPOnlyLayers != nil {
		base.MLPOnlyLayers = nested.MLPOnlyLayers
	}
	if nested.NumExperts != nil {
		base.NumExperts = nested.NumExperts
	}
	if nested.NRoutedExperts != nil {
		base.NRoutedExperts = nested.NRoutedExperts
	}
	if nested.NumLocalExperts != nil {
		base.NumLocalExperts = nested.NumLocalExperts
	}
	if len(nested.LayerTypes) > 0 {
		base.LayerTypes = nested.LayerTypes
	}
	if len(nested.Architectures) > 0 {
		base.Architectures = nested.Architectures
	}
	if nested.ModelType != "" {
		base.ModelType = nested.ModelType
	}
	if nested.TorchDtype != "" {
		base.TorchDtype = nested.TorchDtype
	}
	if presentJSON(nested.NumParameters) {
		base.NumParameters = nested.NumParameters
	}
	if presentJSON(nested.ParameterCount) {
		base.ParameterCount = nested.ParameterCount
	}
	if nested.QuantizationConfig != nil {
		base.QuantizationConfig = nested.QuantizationConfig
	}
	return base
}

func parameterCount(configs ...rawModelConfig) string {
	for _, config := range configs {
		for _, raw := range []json.RawMessage{config.NumParameters, config.ParameterCount} {
			if value := scalarString(raw); value != "" {
				return value
			}
		}
	}
	return ""
}

func scalarString(raw json.RawMessage) string {
	if !presentJSON(raw) {
		return ""
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func quantizationMethod(configs ...rawModelConfig) string {
	for _, config := range configs {
		for _, key := range []string{"quant_method", "quantization_method"} {
			if value, ok := config.QuantizationConfig[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
	}
	return ""
}

func presentJSON(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) && !bytes.Equal(trimmed, []byte("{}"))
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
