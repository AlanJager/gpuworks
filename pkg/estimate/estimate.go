// Package estimate holds the hardware facts a VRAM estimate needs that are
// properties of the model rather than of a serving engine: what an element of a
// given dtype costs, and what a multimodal tower's runtime cache costs.
//
// It deliberately does not compute KV cache per token. That cost depends on how
// the engine lays the cache out, which no config.json states and which the
// engines disagree about - vLLM charges every layer of a Mamba hybrid against
// the pool while the config's layer_types suggests otherwise. Deriving it here
// meant reimplementing the engines and being wrong about them; the catalog
// carries the number the engine itself produces instead (see
// artifacts/archive/2026-09-13-vram-estimate/plan.md).
package estimate

import (
	"strings"

	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// KVBaselineElementBytes is the element width the catalog's kv_bytes_per_token
// figures are recorded at: 2 bytes, the bf16/fp16 default. A deployment whose
// KV cache is narrower or wider scales the recorded figure by the ratio, which
// is exact arithmetic rather than another estimate - which is why the catalog
// stores one number per model instead of one per dtype.
//
// This constant is a contract with the generator: a value written at any other
// width silently becomes a wrong answer here.
// scripts/estimate-calib/kv_layout.py writes the table at this width (BASE_ELEM).
const KVBaselineElementBytes = 2

// DtypeSizeBytes returns bytes per element for a KV/compute dtype string.
//
//	"float16","bfloat16","fp16","bf16" -> 2
//	"fp8","fp8_*","float8*","e4m3","e5m2","int8" -> 1
//	"float32","fp32"                    -> 4
//	default / "auto"                    -> 2
//
// The comparison is case-insensitive.
func DtypeSizeBytes(dtype string) float64 {
	d := strings.ToLower(strings.TrimSpace(dtype))
	switch d {
	case "float32", "fp32", "f32":
		return 4
	case "float16", "bfloat16", "fp16", "bf16", "f16", "half":
		return 2
	case "fp8", "e4m3", "e5m2", "f8_e4m3", "f8_e5m2", "int8", "i8", "uint8", "u8":
		return 1
	}
	// The deploy form's own options spell this dtype "fp8_e4m3" and "fp8_e5m2",
	// which match none of the exact names above; reading them as unknown would
	// return the 2-byte default and double every fp8 estimate.
	if strings.HasPrefix(d, "fp8") || strings.HasPrefix(d, "float8") {
		return 1
	}
	// default / "auto" / unknown
	return 2
}

// MMEncoderRuntimeBytes estimates the GPU-resident encoder output cache and a
// same-sized execution workspace. Tower parameters are already included in
// WeightBytes and are deliberately not counted again.
func MMEncoderRuntimeBytes(cfg *modelconfig.Config, dtype string) int64 {
	if cfg == nil || !cfg.HasMultimodalTower() {
		return 0
	}
	hidden := cfg.HiddenSize
	if hidden <= 0 && cfg.TextConfig != nil {
		hidden = cfg.TextConfig.HiddenSize
	}
	if hidden <= 0 {
		hidden = 4096
	}
	if hidden > 65536 {
		hidden = 65536
	}
	bytesPerElement := int64(DtypeSizeBytes(dtype))
	if bytesPerElement <= 0 {
		bytesPerElement = 2
	}
	var tokens int64
	if vision := cfg.VisionConfig; vision != nil {
		visionTokens := 1024
		if vision.ImageSize > 0 && vision.PatchSize > 0 {
			grid := (vision.ImageSize + vision.PatchSize - 1) / vision.PatchSize
			visionTokens = grid*grid + 1
		} else if vision.MaxPositionEmbeddings > 0 {
			visionTokens = vision.MaxPositionEmbeddings
		}
		tokens += int64(clampTowerTokens(visionTokens))
	}
	if audio := cfg.AudioConfig; audio != nil {
		audioTokens := audio.MaxSourcePositions
		if audioTokens <= 0 {
			audioTokens = audio.MaxPositionEmbeddings
		}
		if audioTokens <= 0 {
			audioTokens = 1500
		}
		tokens += int64(clampTowerTokens(audioTokens))
	}
	return tokens * int64(hidden) * bytesPerElement * 2
}

func clampTowerTokens(tokens int) int {
	if tokens < 1 {
		return 1
	}
	if tokens > 65536 {
		return 65536
	}
	return tokens
}
