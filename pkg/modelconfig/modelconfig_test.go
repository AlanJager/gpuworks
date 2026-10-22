package modelconfig

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		json string
		want func(t *testing.T, c *Config)
	}{
		{
			name: "GQA Qwen-like derived head_dim",
			json: `{
				"architectures": ["Qwen2ForCausalLM"],
				"model_type": "qwen2",
				"num_hidden_layers": 28,
				"hidden_size": 3584,
				"num_attention_heads": 28,
				"num_key_value_heads": 4,
				"max_position_embeddings": 32768,
				"vocab_size": 152064,
				"torch_dtype": "bfloat16"
			}`,
			want: func(t *testing.T, c *Config) {
				if got := c.EffectiveKVHeads(); got != 4 {
					t.Errorf("EffectiveKVHeads = %d, want 4", got)
				}
				if c.NumKeyValueHeads >= c.NumAttentionHeads {
					t.Errorf("expected GQA: kv %d < attn %d", c.NumKeyValueHeads, c.NumAttentionHeads)
				}
				// head_dim absent -> derived 3584/28 = 128
				if got := c.EffectiveHeadDim(); got != 128 {
					t.Errorf("EffectiveHeadDim = %d, want 128", got)
				}
				if c.IsMLA() {
					t.Errorf("IsMLA = true, want false")
				}
				if len(c.Architectures) != 1 || c.Architectures[0] != "Qwen2ForCausalLM" {
					t.Errorf("Architectures = %v", c.Architectures)
				}
			},
		},
		{
			name: "MLA DeepSeek-like",
			json: `{
				"architectures": ["DeepseekV3ForCausalLM"],
				"model_type": "deepseek_v3",
				"num_hidden_layers": 61,
				"hidden_size": 7168,
				"num_attention_heads": 128,
				"kv_lora_rank": 512,
				"qk_rope_head_dim": 64,
				"qk_nope_head_dim": 128,
				"v_head_dim": 128,
				"torch_dtype": "bfloat16"
			}`,
			want: func(t *testing.T, c *Config) {
				if !c.IsMLA() {
					t.Errorf("IsMLA = false, want true")
				}
				if c.KVLoraRank != 512 {
					t.Errorf("KVLoraRank = %d, want 512", c.KVLoraRank)
				}
				if c.QKRopeHeadDim != 64 {
					t.Errorf("QKRopeHeadDim = %d, want 64", c.QKRopeHeadDim)
				}
				if c.QKNopeHeadDim != 128 {
					t.Errorf("QKNopeHeadDim = %d, want 128", c.QKNopeHeadDim)
				}
				if c.VHeadDim != 128 {
					t.Errorf("VHeadDim = %d, want 128", c.VHeadDim)
				}
			},
		},
		{
			name: "nested text_config promoted",
			json: `{
				"architectures": ["Gemma3ForConditionalGeneration"],
				"model_type": "gemma3",
				"text_config": {
					"model_type": "gemma3_text",
					"num_hidden_layers": 34,
					"hidden_size": 3840,
					"num_attention_heads": 16,
					"num_key_value_heads": 8,
					"head_dim": 256,
					"max_position_embeddings": 131072,
					"vocab_size": 262208,
					"torch_dtype": "bfloat16"
				}
			}`,
			want: func(t *testing.T, c *Config) {
				if c.NumHiddenLayers != 34 {
					t.Errorf("NumHiddenLayers = %d, want 34 (promoted)", c.NumHiddenLayers)
				}
				if c.HiddenSize != 3840 {
					t.Errorf("HiddenSize = %d, want 3840 (promoted)", c.HiddenSize)
				}
				if c.EffectiveKVHeads() != 8 {
					t.Errorf("EffectiveKVHeads = %d, want 8 (promoted)", c.EffectiveKVHeads())
				}
				if c.EffectiveHeadDim() != 256 {
					t.Errorf("EffectiveHeadDim = %d, want 256 (promoted)", c.EffectiveHeadDim())
				}
				if c.MaxPositionEmbeddings != 131072 {
					t.Errorf("MaxPositionEmbeddings = %d, want 131072 (promoted)", c.MaxPositionEmbeddings)
				}
				if c.VocabSize != 262208 {
					t.Errorf("VocabSize = %d, want 262208 (promoted)", c.VocabSize)
				}
				if c.TorchDtype != "bfloat16" {
					t.Errorf("TorchDtype = %q, want bfloat16 (promoted)", c.TorchDtype)
				}
				// outer model_type is non-empty, must NOT be overwritten
				if c.ModelType != "gemma3" {
					t.Errorf("ModelType = %q, want gemma3 (outer preserved)", c.ModelType)
				}
				if c.TextConfig == nil {
					t.Errorf("TextConfig should still be attached")
				} else if c.TextConfig.ModelType != "gemma3_text" {
					t.Errorf("TextConfig.ModelType = %q, want gemma3_text", c.TextConfig.ModelType)
				}
			},
		},
		{
			name: "omni text and encoder towers preserved",
			json: `{
				"architectures": ["SomeOmniForConditionalGeneration"],
				"model_type": "some_omni",
				"text_config": {
					"hidden_size": 4096,
					"num_hidden_layers": 32,
					"num_attention_heads": 32,
					"max_position_embeddings": 32768,
					"torch_dtype": "bfloat16"
				},
				"vision_config": {
					"model_type": "vision_tower",
					"hidden_size": 1024,
					"image_size": 448,
					"patch_size": 14
				},
				"audio_config": {
					"model_type": "audio_tower",
					"d_model": 1280,
					"max_source_positions": 1500,
					"num_mel_bins": 128
				}
			}`,
			want: func(t *testing.T, c *Config) {
				if c.HiddenSize != 4096 || c.NumHiddenLayers != 32 || c.NumAttentionHeads != 32 {
					t.Fatalf("text config was not promoted: %+v", c)
				}
				if c.MaxPositionEmbeddings != 32768 || c.TorchDtype != "bfloat16" {
					t.Fatalf("text limits/dtype were not promoted: %+v", c)
				}
				if !c.HasMultimodalTower() || c.VisionConfig == nil || c.AudioConfig == nil {
					t.Fatalf("multimodal towers were discarded: %+v", c)
				}
				if c.VisionConfig.ImageSize != 448 || c.VisionConfig.PatchSize != 14 {
					t.Fatalf("vision config = %+v", c.VisionConfig)
				}
				if c.AudioConfig.DModel != 1280 || c.AudioConfig.MaxSourcePositions != 1500 || c.AudioConfig.NumMelBins != 128 {
					t.Fatalf("audio config = %+v", c.AudioConfig)
				}
			},
		},
		{
			name: "Qwen2.5 Omni thinker config promoted",
			json: `{
				"architectures": ["Qwen2_5OmniModel"],
				"model_type": "qwen2_5_omni",
				"thinker_config": {
					"model_type": "qwen2_5_omni_thinker",
					"torch_dtype": "bfloat16",
					"text_config": {
						"model_type": "qwen2_5_omni_text",
						"hidden_size": 3584,
						"num_hidden_layers": 28,
						"num_attention_heads": 28,
						"num_key_value_heads": 4,
						"max_position_embeddings": 32768
					},
					"vision_config": {
						"model_type": "qwen2_5_omni_vision_encoder",
						"hidden_size": 1280,
						"embed_dim": 1280,
						"out_hidden_size": 3584,
						"depth": 32,
						"patch_size": 14,
						"spatial_merge_size": 2
					},
					"audio_config": {
						"model_type": "qwen2_5_omni_audio_encoder",
						"d_model": 1280,
						"encoder_layers": 32,
						"max_source_positions": 1500,
						"num_mel_bins": 128,
						"output_dim": 3584
					}
				}
			}`,
			want: func(t *testing.T, c *Config) {
				if c.ModelType != "qwen2_5_omni" || c.HiddenSize != 3584 || c.NumHiddenLayers != 28 || c.NumKeyValueHeads != 4 {
					t.Fatalf("Qwen Omni text config not promoted: %+v", c)
				}
				if c.TorchDtype != "bfloat16" || !c.HasMultimodalTower() {
					t.Fatalf("Qwen Omni thinker dtype/towers missing: %+v", c)
				}
				if c.ThinkerConfig == nil || c.TextConfig == nil || c.VisionConfig == nil || c.AudioConfig == nil {
					t.Fatalf("Qwen Omni nested configs not preserved: %+v", c)
				}
				if c.VisionConfig.Depth != 32 || c.VisionConfig.OutHiddenSize != 3584 || c.VisionConfig.SpatialMergeSize != 2 {
					t.Fatalf("Qwen Omni vision config = %+v", c.VisionConfig)
				}
				if c.AudioConfig.EncoderLayers != 32 || c.AudioConfig.OutputDim != 3584 || c.AudioConfig.MaxSourcePositions != 1500 {
					t.Fatalf("Qwen Omni audio config = %+v", c.AudioConfig)
				}
			},
		},
		{
			name: "rope_scaling parsed as map and sliding_window",
			json: `{
				"model_type": "llama",
				"num_hidden_layers": 32,
				"hidden_size": 4096,
				"num_attention_heads": 32,
				"num_key_value_heads": 8,
				"sliding_window": 4096,
				"layer_types": ["sliding_attention", "full_attention"],
				"rope_scaling": {
					"rope_type": "llama3",
					"factor": 8.0,
					"high_freq_factor": 4.0,
					"low_freq_factor": 1.0,
					"original_max_position_embeddings": 8192
				}
			}`,
			want: func(t *testing.T, c *Config) {
				if c.RopeScaling == nil {
					t.Fatalf("RopeScaling is nil")
				}
				if rt, ok := c.RopeScaling["rope_type"].(string); !ok || rt != "llama3" {
					t.Errorf("rope_scaling.rope_type = %v, want llama3", c.RopeScaling["rope_type"])
				}
				if f, ok := c.RopeScaling["factor"].(float64); !ok || f != 8.0 {
					t.Errorf("rope_scaling.factor = %v, want 8.0", c.RopeScaling["factor"])
				}
				if c.SlidingWindow != 4096 {
					t.Errorf("SlidingWindow = %d, want 4096", c.SlidingWindow)
				}
				if len(c.LayerTypes) != 2 || c.LayerTypes[0] != "sliding_attention" {
					t.Errorf("LayerTypes = %v", c.LayerTypes)
				}
				// no head_dim, derive 4096/32 = 128
				if got := c.EffectiveHeadDim(); got != 128 {
					t.Errorf("EffectiveHeadDim = %d, want 128", got)
				}
			},
		},
		{
			name: "MQA absent kv heads falls back to attn heads",
			json: `{
				"model_type": "gpt2",
				"num_attention_heads": 12,
				"hidden_size": 768
			}`,
			want: func(t *testing.T, c *Config) {
				if got := c.EffectiveKVHeads(); got != 12 {
					t.Errorf("EffectiveKVHeads = %d, want 12 (fallback)", got)
				}
				if got := c.EffectiveHeadDim(); got != 64 {
					t.Errorf("EffectiveHeadDim = %d, want 64", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Parse(strings.NewReader(tt.json))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			tt.want(t, c)
		})
	}
}

func TestParseError(t *testing.T) {
	if _, err := Parse(strings.NewReader("{not json")); err == nil {
		t.Errorf("expected error for invalid JSON, got nil")
	}
}

func TestEffectiveHeadDimZeroHeads(t *testing.T) {
	c := &Config{HiddenSize: 4096}
	if got := c.EffectiveHeadDim(); got != 0 {
		t.Errorf("EffectiveHeadDim with 0 attention heads = %d, want 0", got)
	}
}
