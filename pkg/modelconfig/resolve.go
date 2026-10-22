package modelconfig

// nestedLanguageKeys are the places repositories hide the language model when
// the top level carries only wrapper metadata. Order is preference order: the
// HuggingFace standard spelling wins, and a field is only taken from a later
// level when every earlier level left it unset.
var nestedLanguageKeys = []string{
	"text_config",     // Qwen-VL/Audio, Gemma 3, Llama 4, Cosmos
	"thinker_config",  // Qwen2.5-Omni
	"llm_config",      // Ovis, InternVL
	"lm_config",       // VoxCPM2
	"language_config", // MOSS-TTS
	"language_model",  // Janus-Pro, DeepSeek-VL2
	"talker_config",   // Qwen3-TTS
	"generator",       // speech/audio pipelines
	"decoder",         // encoder-decoder wrappers
}

// FromMap builds a Config from already-decoded config.json fields.
//
// A document can describe several transformer stacks: the model itself, plus a
// talker, a draft head or a code predictor, each with its own depth and width.
// The estimator sizes exactly one of them, so the stack is chosen first and the
// other levels may only fill fields that stack left unset — a talker must not
// lend its head size to a thinker.
func FromMap(raw map[string]any) *Config {
	candidates := []*Config{levelConfig(raw)}
	for _, key := range nestedLanguageKeys {
		if nested, ok := raw[key].(map[string]any); ok {
			candidates = append(candidates, nestedLevel(nested))
		}
	}
	primary := primaryLevel(candidates)
	cfg := *primary
	if primary != candidates[0] {
		// The document names the model; a nested level names only the component
		// it holds (qwen2_5_omni_thinker inside qwen2_5_omni).
		if document := candidates[0]; document.ModelType != "" {
			cfg.ModelType = document.ModelType
		}
		if document := candidates[0]; len(document.Architectures) > 0 {
			cfg.Architectures = document.Architectures
		}
	}
	for _, candidate := range candidates {
		if candidate == primary || !sameStack(primary, candidate) {
			continue
		}
		promoteShape(&cfg, candidate)
	}
	for _, candidate := range candidates {
		promoteContext(&cfg, candidate)
	}
	// Existing readers size the multimodal encoder runtime from these two, so
	// they stay available as their own configs.
	if text, ok := raw["text_config"].(map[string]any); ok {
		cfg.TextConfig = levelConfig(text)
	}
	if thinker, ok := raw["thinker_config"].(map[string]any); ok {
		cfg.ThinkerConfig = levelConfig(thinker)
		if cfg.TextConfig == nil {
			if text, ok := thinker["text_config"].(map[string]any); ok {
				cfg.TextConfig = levelConfig(text)
			}
		}
	}
	return &cfg
}

// nestedLevel reads one nested language config, folding in the text_config it
// may itself carry (thinker_config.text_config, for example).
func nestedLevel(m map[string]any) *Config {
	nested := levelConfig(m)
	if text, ok := m["text_config"].(map[string]any); ok {
		promoteShape(nested, levelConfig(text))
		promoteContext(nested, levelConfig(text))
	}
	return nested
}

// primaryLevel picks the level that describes the stack to size: the most
// complete one, and among equals the one that comes first in preference order.
func primaryLevel(candidates []*Config) *Config {
	best := candidates[0]
	bestScore := stackScore(best)
	for _, candidate := range candidates[1:] {
		if score := stackScore(candidate); score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best
}

// stackScore counts what a level says about the attention stack. Layers and
// heads decide whether it describes one at all.
func stackScore(c *Config) int {
	score := 0
	if c.NumHiddenLayers > 0 {
		score += 8
	}
	if c.NumAttentionHeads > 0 {
		score += 4
	}
	if c.IsMLA() {
		score += 4
	}
	if c.HiddenSize > 0 {
		score += 2
	}
	if c.NumKeyValueHeads > 0 {
		score++
	}
	return score
}

// sameStack reports whether two levels describe the same stack. A level that
// declares no layer count is treated as complementary — it may be a repository
// splitting one stack across keys — while a different count is another stack.
func sameStack(primary, candidate *Config) bool {
	return primary.NumHiddenLayers == 0 || candidate.NumHiddenLayers == 0 ||
		primary.NumHiddenLayers == candidate.NumHiddenLayers
}

// promoteShape copies the attention stack a level declares onto dst, leaving
// fields dst already has. It is the single place that decides which shape fields
// may travel between levels, so a new estimation field cannot be wired into one
// path only.
func promoteShape(dst, src *Config) {
	if dst == nil || src == nil {
		return
	}
	for _, field := range []struct{ dst, src *int }{
		{&dst.NumHiddenLayers, &src.NumHiddenLayers},
		{&dst.HiddenSize, &src.HiddenSize},
		{&dst.NumAttentionHeads, &src.NumAttentionHeads},
		{&dst.NumKeyValueHeads, &src.NumKeyValueHeads},
		{&dst.HeadDim, &src.HeadDim},
		{&dst.SlidingWindow, &src.SlidingWindow},
		{&dst.KVLoraRank, &src.KVLoraRank},
		{&dst.QKRopeHeadDim, &src.QKRopeHeadDim},
		{&dst.QKNopeHeadDim, &src.QKNopeHeadDim},
		{&dst.VHeadDim, &src.VHeadDim},
	} {
		if *field.dst == 0 {
			*field.dst = *field.src
		}
	}
	if len(dst.LayerTypes) == 0 {
		dst.LayerTypes = src.LayerTypes
	}
	if dst.UseSlidingWindow == nil {
		dst.UseSlidingWindow = src.UseSlidingWindow
	}
}

// promoteContext copies the facts that describe the model rather than one stack:
// dtype, context window, vocabulary, and the multimodal towers. Any level may
// carry them.
func promoteContext(dst, src *Config) {
	if dst == nil || src == nil {
		return
	}
	if len(dst.Architectures) == 0 {
		dst.Architectures = src.Architectures
	}
	if dst.ModelType == "" {
		dst.ModelType = src.ModelType
	}
	if dst.TorchDtype == "" {
		dst.TorchDtype = src.TorchDtype
	}
	if dst.MaxPositionEmbeddings == 0 {
		dst.MaxPositionEmbeddings = src.MaxPositionEmbeddings
	}
	if dst.VocabSize == 0 {
		dst.VocabSize = src.VocabSize
	}
	if dst.RopeScaling == nil {
		dst.RopeScaling = src.RopeScaling
	}
	if dst.KVCache == nil {
		dst.KVCache = src.KVCache
	}
	if dst.VisionConfig == nil {
		dst.VisionConfig = src.VisionConfig
	}
	if dst.AudioConfig == nil {
		dst.AudioConfig = src.AudioConfig
	}
}

// levelConfig reads one nesting level. Aliases are resolved here so every level
// accepts the same spellings.
func levelConfig(m map[string]any) *Config {
	if m == nil {
		return &Config{}
	}
	c := &Config{
		Architectures:         stringList(m, "architectures"),
		ModelType:             stringOf(m, "model_type"),
		NumHiddenLayers:       intOf(m, "num_hidden_layers", "num_layers", "n_layers", "n_layer"),
		HiddenSize:            intOf(m, "hidden_size", "d_model", "n_embd", "model_dim", "dim"),
		NumAttentionHeads:     intOf(m, "num_attention_heads", "n_head", "num_heads", "n_heads"),
		NumKeyValueHeads:      intOf(m, "num_key_value_heads", "num_kv_heads", "n_head_kv", "kv_heads"),
		HeadDim:               intOf(m, "head_dim", "attention_head_dim", "head_size", "d_kv"),
		MaxPositionEmbeddings: intOf(m, "max_position_embeddings", "n_positions", "max_sequence_length", "seq_length"),
		VocabSize:             intOf(m, "vocab_size"),
		TorchDtype:            stringOf(m, "torch_dtype", "dtype"),
		RopeScaling:           mapOf(m, "rope_scaling"),
		SlidingWindow:         intOf(m, "sliding_window"),
		UseSlidingWindow:      boolOf(m, "use_sliding_window"),
		KVLoraRank:            intOf(m, "kv_lora_rank"),
		QKRopeHeadDim:         intOf(m, "qk_rope_head_dim"),
		QKNopeHeadDim:         intOf(m, "qk_nope_head_dim"),
		VHeadDim:              intOf(m, "v_head_dim"),
		LayerTypes:            layerTypes(m),
		KVCache:               boolOf(m, "kv_cache"),
		VisionConfig:          towerOf(m, "vision_config", "vit_config", "vision_encoder"),
		AudioConfig:           towerOf(m, "audio_config"),
	}
	applyDecoderStack(c, m)
	return c
}

// applyDecoderStack points the estimate at the stack that actually keeps a KV
// cache. Encoder-decoder configs (Whisper and relatives) count encoder layers in
// num_hidden_layers and name their heads decoder_attention_heads, so when the
// level itself never named attention heads the decoder triple is the language
// model we must size.
func applyDecoderStack(c *Config, m map[string]any) {
	if c.NumAttentionHeads > 0 {
		return
	}
	heads := intOf(m, "decoder_attention_heads", "num_decoder_attention_heads")
	if heads <= 0 {
		return
	}
	c.NumAttentionHeads = heads
	if layers := intOf(m, "decoder_layers", "num_decoder_layers", "n_decoder_layers"); layers > 0 {
		c.NumHiddenLayers = layers
	}
	c.HeadDim = 0 // re-derived from the decoder width below
	if width := intOf(m, "d_model", "hidden_size", "decoder_hidden_size"); width > 0 {
		c.HiddenSize = width
	}
	c.NumKeyValueHeads = 0
}

// layerTypes accepts the list spellings used by hybrid attention models plus the
// compact pattern strings (Jamba, Nemotron) and the per-layer index lists some
// vendors publish instead of any of those.
func layerTypes(m map[string]any) []string {
	if list := stringList(m, "layer_types", "layers_block_type", "layer_block_type", "block_types"); len(list) > 0 {
		return list
	}
	for _, key := range []string{"hybrid_override_pattern", "hybrid_layer_pattern", "layer_pattern"} {
		if pattern := stringOf(m, key); pattern != "" {
			return expandLayerPattern(pattern)
		}
	}
	if linear, ok := m["linear_attn_config"].(map[string]any); ok {
		return linearAttentionLayers(m, linear)
	}
	return nil
}

// expandLayerPattern turns a compact hybrid pattern into layer type names. One
// character is one layer: '*' attention, 'M' Mamba, '-' a feed-forward layer.
// Unrecognised characters are passed through so an unknown layer kind is still
// visible to the layer classifier instead of being silently dropped.
func expandLayerPattern(pattern string) []string {
	out := make([]string, 0, len(pattern))
	for _, r := range pattern {
		switch r {
		case '_', ' ':
			continue
		case '-':
			out = append(out, "mlp")
		case '*':
			out = append(out, "full_attention")
		case 'M', 'm':
			out = append(out, "mamba")
		default:
			out = append(out, string(r))
		}
	}
	return out
}

// linearAttentionLayers builds the per-layer list from a vendor's index lists
// (Kimi-Linear and relatives). Layers the declaration does not name stay
// unknown, which the layer classifier counts as attention rather than dropping
// them from the estimate.
func linearAttentionLayers(level, linear map[string]any) []string {
	full := intList(linear, "full_attn_layers", "full_attention_layers")
	if len(full) == 0 {
		return nil
	}
	known := make(map[int]bool, len(full))
	for _, index := range full {
		known[index] = true
	}
	// The published indices are one-based layer numbers.
	layers := intOf(level, "num_hidden_layers", "num_layers", "n_layers", "n_layer")
	last := 0
	for index := range known {
		if index > last {
			last = index
		}
	}
	if layers < last {
		layers = last
	}
	out := make([]string, 0, layers)
	for index := 1; index <= layers; index++ {
		if known[index] {
			out = append(out, "full_attention")
		} else {
			out = append(out, "linear_attention")
		}
	}
	return out
}
