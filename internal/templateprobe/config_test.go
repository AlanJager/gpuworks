package templateprobe

import "testing"

func TestParseModelConfigPromotesNestedTextAndMoEFields(t *testing.T) {
	raw := []byte(`{
  "architectures":["QwenOmniForConditionalGeneration"],
  "model_type":"qwen_omni",
  "vision_config":{"hidden_size":128},
  "thinker_config":{"text_config":{
    "num_hidden_layers":48,
    "max_position_embeddings":65536,
    "decoder_sparse_step":4,
    "mlp_only_layers":[3],
    "num_experts":64,
    "layer_types":["attention","mamba","attention"],
    "torch_dtype":"bfloat16",
    "num_parameters":123456
  }}
}`)
	config, err := ParseModelConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if config.NumHiddenLayers != 48 || config.MaxPositionEmbedding != 65536 {
		t.Fatalf("nested language config not promoted: %+v", config)
	}
	if config.DecoderSparseStep == nil || *config.DecoderSparseStep != 4 || config.NumExperts == nil || *config.NumExperts != 64 {
		t.Fatalf("nested MoE config not promoted: %+v", config)
	}
	if config.ParameterCount != "123456" || !config.Multimodal {
		t.Fatalf("metadata = %+v, want explicit parameter count and multimodal", config)
	}
}

func TestDecideTruncationSpecificationCases(t *testing.T) {
	integer := func(value int) *int { return &value }
	tests := []struct {
		name    string
		config  ModelConfig
		allowed bool
		kind    TruncationKind
		layers  int
	}{
		{"dense", ModelConfig{NumHiddenLayers: 32}, true, TruncationDense, 2},
		{"deepseek moe", ModelConfig{NumHiddenLayers: 61, FirstKDenseReplace: integer(3), MoELayerFreq: integer(1), NRoutedExperts: integer(256)}, true, TruncationDeepSeekMoE, 5},
		{"qwen3 moe skips mlp-only candidate", ModelConfig{NumHiddenLayers: 48, DecoderSparseStep: integer(4), MLPOnlyLayers: []int{3}, NumExperts: integer(64)}, true, TruncationQwenMoE, 9},
		{"hybrid prefix plus one", ModelConfig{NumHiddenLayers: 4, LayerTypes: []string{"attention", "attention", "mamba", "attention"}}, true, TruncationHybrid, 4},
		{"multimodal text tower", ModelConfig{NumHiddenLayers: 28, Multimodal: true}, true, TruncationDense, 2},
		{"unknown moe refused", ModelConfig{NumHiddenLayers: 40, NumLocalExperts: integer(8)}, false, TruncationUnknownMoE, 0},
		{"incomplete hybrid map refused", ModelConfig{NumHiddenLayers: 24, LayerTypes: []string{"attention", "mamba"}}, false, TruncationHybrid, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := DecideTruncation(test.config)
			if got.Allowed != test.allowed || got.Kind != test.kind || got.MinLayers != test.layers {
				t.Fatalf("DecideTruncation() = %+v", got)
			}
		})
	}
}
