// Command gocalc prints the current estimator's VRAM breakdown for a model
// config on disk, so a measurement can be diffed against what the product
// would have told the user for the exact same inputs.
//
//	go run ./scripts/estimate-calib/gocalc -config <dir-or-config.json> \
//	  -weights-bytes N -gpu-bytes N -util 0.9 -tp 1 -maxlen 4096 -engine vllm
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	srv "gpuworks.ai/gpuworks/internal/server/estimate"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

func main() {
	cfgPath := flag.String("config", "", "model dir or config.json path")
	weights := flag.Int64("weights-bytes", 0, "weight bytes (on-disk artifact size)")
	componentWeights := flag.Int64("component-weight-bytes", 0, "extra component weight bytes")
	gpuBytes := flag.Int64("gpu-bytes", 0, "per-GPU total memory bytes")
	vramLimit := flag.Int64("vram-limit-bytes", 0, "per-GPU VRAM limit (partition), 0 = none")
	util := flag.Float64("util", 0.9, "gpu_memory_utilization")
	tp := flag.Int("tp", 1, "tensor parallel size")
	maxLen := flag.Int("maxlen", 0, "max_model_len (0 = config default)")
	engine := flag.String("engine", "vllm", "engine name")
	dtype := flag.String("dtype", "", "model dtype")
	kvDtype := flag.String("kv-dtype", "auto", "kv cache dtype")
	flag.Parse()

	path := *cfgPath
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "config.json")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read config:", err)
		os.Exit(1)
	}
	cfg, err := modelconfig.Parse(bytes.NewReader(raw))
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse config:", err)
		os.Exit(1)
	}

	// The reserve base comes from the engine's own profile, the same fact the
	// server charges, so a calibration run and a deployment cannot disagree.
	reserveBase := int64(0)
	if profile, err := deploy.ProfileForEngine(*engine); err == nil {
		reserveBase = profile.ReserveBase
	}
	b := srv.Compute(srv.Params{
		Config: cfg, ReserveBase: reserveBase, WeightBytes: *weights,
		ComponentWeightBytes: *componentWeights, KVDtype: *kvDtype, Dtype: *dtype,
		MaxModelLen: *maxLen, TP: *tp, GPUMemUtil: *util,
		VRAMLimitBytes: *vramLimit, GPUCapacityBytes: *gpuBytes,
	})

	// The config facts the KV math depends on, so a mismatch can be attributed
	// to extraction rather than to the formula.
	facts := map[string]any{
		"num_hidden_layers":   cfg.NumHiddenLayers,
		"hidden_size":         cfg.HiddenSize,
		"num_attention_heads": cfg.NumAttentionHeads,
		"num_key_value_heads": cfg.NumKeyValueHeads,
		"head_dim":            cfg.HeadDim,
		"effective_kv_heads":  cfg.EffectiveKVHeads(),
		"effective_head_dim":  cfg.EffectiveHeadDim(),
		"is_mla":              cfg.IsMLA(),
		"kv_lora_rank":        cfg.KVLoraRank,
		"qk_rope_head_dim":    cfg.QKRopeHeadDim,
		"sliding_window":      cfg.SlidingWindow,
		"layer_types":         cfg.LayerTypes,
		"max_position_emb":    cfg.MaxPositionEmbeddings,
		"caches_kv_tokens":    cfg.CachesKVTokens(),
	}
	out := map[string]any{"breakdown": b, "config_facts": facts}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	_ = enc.Encode(out)
}
