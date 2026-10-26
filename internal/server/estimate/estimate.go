// Package estimate is the server-side façade over pkg/estimate: it turns a
// model config + params into a VRAM breakdown (for the deploy form and
// deployment detail) and into scheduler request inputs. It also owns the dtype
// hardware matrix.
package estimate

import (
	"strings"

	pkgest "gpuworks.ai/gpuworks/pkg/estimate"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// Grade strings the deploy form renders. A measured KV cost is the only exact
// answer this package can give, because everything else is a sizing rule.
const (
	GradeMeasured = "measured"
	// GradeNotApplicable means the question does not apply: the model keeps no
	// token-indexed KV cache, so no pool can be sized. A confident answer rather
	// than a missing one.
	GradeNotApplicable = "not_applicable"
	GradeRoughUpper    = "rough_upper"
)

// Why a breakdown reports no token capacity. These are stable codes rather than
// sentences: the reader's locale belongs to the client, and a server sentence
// rendered inside a translated frame is how a Chinese page came to say "暂无法
// 估算 token 容量（no measured KV footprint for this model）".
const (
	// KVReasonNoMeasurement means the catalog carries no figure for this model
	// on this engine, and gpuworks will not invent one.
	KVReasonNoMeasurement = "kv_no_measurement"
	// KVReasonStartupExceedsCapacity means the model does not fit at all, so
	// there is no pool left to report. Distinguished from a measured zero.
	KVReasonStartupExceedsCapacity = "kv_startup_exceeds_capacity"
	// KVReasonNotApplicable means the model keeps no token-indexed KV cache.
	KVReasonNotApplicable = "kv_not_applicable"
)

// Breakdown is the UI-facing VRAM decomposition (bytes; UI converts to GiB).
type Breakdown struct {
	WeightBytes          int64            `json:"weight_bytes"`
	ComponentWeightBytes int64            `json:"component_weight_bytes,omitempty"`
	PerDtype             map[string]int64 `json:"per_dtype,omitempty"`
	KVBytesPerToken      float64          `json:"kv_bytes_per_token"`
	KVTotalBytes         int64            `json:"kv_total_bytes"`
	// ReserveBytes is the engine's non-weight, non-KV memory: one figure, not a
	// decomposition, because gpuworks deliberately does not model an engine's
	// internals (see ReserveBytes).
	ReserveBytes             int64   `json:"reserve_bytes"`
	MMEncoderBytes           int64   `json:"mm_encoder_bytes"`
	PerGPUBytes              int64   `json:"per_gpu_bytes"`
	StartupFixedBytes        int64   `json:"startup_fixed_bytes"`
	TotalBytes               int64   `json:"total_bytes"`
	ActualTotalBytes         int64   `json:"actual_total_bytes"`
	Grade                    string  `json:"grade"` // measured|rough_upper|not_applicable
	TensorParallel           int     `json:"tensor_parallel"`
	MaxModelLen              int     `json:"max_model_len"`
	Source                   string  `json:"source"` // estimate|catalog
	RuntimeMemoryBytesPerGPU int64   `json:"runtime_memory_bytes_per_gpu,omitempty"`
	GPUMemUtil               float64 `json:"gpu_memory_utilization,omitempty"`
	VRAMLimitBytes           int64   `json:"vram_limit_bytes,omitempty"`
	CapacityBytes            int64   `json:"capacity_bytes,omitempty"`
	KVPoolBytes              int64   `json:"kv_pool_bytes,omitempty"`
	EstimatedKVTokens        int     `json:"estimated_kv_tokens,omitempty"`
	KVEstimateConfidence     string  `json:"kv_estimate_confidence,omitempty"`
	KVEstimateReason         string  `json:"kv_estimate_reason,omitempty"`
	Fits                     bool    `json:"fits"`
	// MaxConcurrency is how many requests of MaxModelLen fit in the KV pool.
	// Nil was not computed (no measured KV cost, or no context length). A
	// pointer to zero is a real zero and has to stay on the wire: omitempty
	// on a plain int hid the deployments that cannot serve even one request.
	MaxConcurrency *int `json:"max_concurrency,omitempty"`
}

// Params are the estimation inputs.
type Params struct {
	Config *modelconfig.Config
	// ReserveBase is the engine's own declared holdback (deploy.Profile.
	// ReserveBase). It only matters to a caller that did not compute
	// ReserveBytes; zero means the engine declares none and the measured
	// default model applies.
	ReserveBase          int64
	WeightBytes          int64
	ComponentWeightBytes int64
	PerDtype             map[string]int64
	KVDtype              string
	Dtype                string
	// KVBytesPerToken is the measured KV cost per token across the whole
	// tensor-parallel group, carried by the catalog. Zero means unmeasured, and
	// the breakdown then reports no token capacity rather than a derived one.
	KVBytesPerToken int64
	// ReserveBytes is the engine's non-weight, non-KV memory for this
	// deployment, from ReserveBytes. Planning computes it once and hands the same
	// value to the scheduler, so what this renders is what admission enforced.
	ReserveBytes             int64
	MaxModelLen              int
	TP                       int
	GPUMemUtil               float64
	VRAMLimitBytes           int64
	GPUCapacityBytes         int64
	RuntimeMemoryBytesPerGPU int64
}

// Compute produces a Breakdown. If cfg is nil, KV is left zero (weights only).
func Compute(p Params) Breakdown {
	tp := p.TP
	if tp <= 0 {
		tp = 1
	}
	maxLen := p.MaxModelLen
	if maxLen <= 0 && p.Config != nil {
		maxLen = p.Config.MaxPositionEmbeddings
	}
	reserve := p.ReserveBytes
	if reserve <= 0 {
		// A caller that did not compute one still gets the conservative branch
		// rather than none: charging no reserve is the direction that admits a
		// deployment the engine then cannot start.
		reserve = ReserveBytes(p.ReserveBase, "", true, p.GPUMemUtil)
	}
	b := Breakdown{
		WeightBytes:              p.WeightBytes + p.ComponentWeightBytes,
		ComponentWeightBytes:     p.ComponentWeightBytes,
		PerDtype:                 p.PerDtype,
		ReserveBytes:             reserve,
		MMEncoderBytes:           pkgest.MMEncoderRuntimeBytes(p.Config, p.Dtype),
		TensorParallel:           tp,
		MaxModelLen:              maxLen,
		Grade:                    GradeRoughUpper,
		Source:                   "estimate",
		GPUMemUtil:               p.GPUMemUtil,
		VRAMLimitBytes:           p.VRAMLimitBytes,
		RuntimeMemoryBytesPerGPU: p.RuntimeMemoryBytesPerGPU,
	}
	// A partition's VRAM limit is an explicit budget; a whole-GPU total is the
	// platform's reading of the card. The slice a driver keeps for itself is
	// covered by the reserve rather than by shrinking this number.
	capacity := p.GPUCapacityBytes
	if p.VRAMLimitBytes > 0 {
		capacity = p.VRAMLimitBytes
	}
	if capacity > 0 {
		b.CapacityBytes = int64(float64(capacity) * b.GPUMemUtil)
	}
	// The KV cost per token is measured from the engine and recorded in the
	// catalog, not derived from config.json: the layout is an engine property
	// that no config states. See docs/reference/inference-catalog-spec.md
	// (kv_bytes_per_token).
	if p.KVBytesPerToken > 0 {
		perToken := float64(p.KVBytesPerToken) * kvWidthScale(p.KVDtype, p.Dtype)
		b.KVBytesPerToken = perToken
		if maxLen > 0 {
			b.KVTotalBytes = int64(perToken * float64(maxLen))
		}
		b.Grade = GradeMeasured
	} else if p.Config != nil && !p.Config.CachesKVTokens() {
		// A model that keeps no token-indexed KV cache has a final answer, not a
		// missing one: the catalog says so, and the deploy form should say it
		// rather than leave the reader wondering whether a measurement is
		// pending.
		b.Grade = GradeNotApplicable
		b.KVEstimateReason = KVReasonNotApplicable
	}
	if b.MMEncoderBytes > 0 {
		// The encoder cache is still a sizing rule rather than a measurement, so
		// it caps how confident the breakdown as a whole can be.
		b.Grade = GradeRoughUpper
	}
	if b.RuntimeMemoryBytesPerGPU > 0 {
		b.Source = "catalog"
	}
	Finalize(&b)
	return b
}

// kvWidthScale is what the catalog's bf16-baseline KV figure is multiplied by
// for a deployment with this KV dtype.
//
// The deployment's own dtype decides unless --kv-cache-dtype overrides it: that
// is the engine's rule, and it is why a model served in float32 - which is what
// the config.json of a few embedding models asks for - costs twice the recorded
// figure. "auto" and an empty string both mean "follow the compute dtype", so
// the two default paths in the deploy form land on the baseline.
func kvWidthScale(kvDtype, computeDtype string) float64 {
	effective := strings.ToLower(strings.TrimSpace(kvDtype))
	if effective == "" || effective == "auto" {
		effective = computeDtype
	}
	if bytes, ok := kvCacheElementBytes[effective]; ok {
		return bytes / pkgest.KVBaselineElementBytes
	}
	return pkgest.DtypeSizeBytes(effective) / pkgest.KVBaselineElementBytes
}

// kvCacheElementBytes covers the engines' KV-only formats that no compute
// dtype name describes: the element width each stores, without the per-block
// scales (a capacity estimate, not an allocator). The "_nc"/kXvY TurboQuant
// formats split keys and values across two widths.
var kvCacheElementBytes = map[string]float64{
	"mxfp8": 1, "int8_per_token_head": 1,
	"nvfp4": 0.5, "nvfp4_4over6": 0.5, "nvfp4_ds_mla": 0.5, "fp4_mx_block16": 0.5,
	"int4_per_token_head": 0.5, "turboquant_4bit_nc": 0.5, "ultraquant_4bit": 0.5,
	"turboquant_k8v4": 0.75, "turboquant_k3v4_nc": 0.4375, "turboquant_3bit_nc": 0.375,
}

// RequiredPerGPUBytes is the single memory-requirement calculation shared by
// estimation, planning and scheduling. KV cache is deliberately absent: engines
// allocate those blocks dynamically, so they are not an admission requirement.
func RequiredPerGPUBytes(weightBytes, mmEncoderBytes, reserveBytes int64, tp int) int64 {
	if tp <= 0 {
		tp = 1
	}
	return weightBytes/int64(tp) + mmEncoderBytes/int64(tp) + reserveBytes
}

// Finalize recalculates all derived fields after estimated
// components change. CapacityBytes must already be the effective per-GPU
// budget after gpu_memory_utilization is applied.
func Finalize(b *Breakdown) {
	tp := b.TensorParallel
	if tp <= 0 {
		tp = 1
		b.TensorParallel = tp
	}
	b.PerGPUBytes = RequiredPerGPUBytes(b.WeightBytes, b.MMEncoderBytes, b.ReserveBytes, tp)
	b.StartupFixedBytes = b.PerGPUBytes
	if b.RuntimeMemoryBytesPerGPU > 0 {
		b.PerGPUBytes = b.RuntimeMemoryBytesPerGPU
		b.StartupFixedBytes = b.PerGPUBytes
	}
	b.TotalBytes = b.PerGPUBytes * int64(tp)
	b.ActualTotalBytes = b.TotalBytes
	b.KVPoolBytes = 0
	b.Fits = false
	b.MaxConcurrency = nil
	b.EstimatedKVTokens = 0
	if b.CapacityBytes <= 0 {
		return
	}
	fixed := RequiredPerGPUBytes(b.WeightBytes, b.MMEncoderBytes, b.ReserveBytes, tp)
	if b.RuntimeMemoryBytesPerGPU > 0 {
		// A catalog resource contract is an observed total peak, not a component
		// decomposition. Treat it as fixed so the UI does not fabricate a KV pool
		// by subtracting the much larger on-disk artifact size.
		fixed = b.RuntimeMemoryBytesPerGPU
	}
	b.KVPoolBytes = b.CapacityBytes - fixed
	b.Fits = b.PerGPUBytes <= b.CapacityBytes
	if b.KVPoolBytes < 0 {
		b.KVPoolBytes = 0
	}
	if b.KVPoolBytes == 0 && !b.Fits {
		// A zero pool after a failed startup fit is not a measured or usable
		// capacity. Keep the startup failure separate from KV estimation so the
		// UI does not render an apparently precise "0 tokens" result.
		b.KVEstimateReason = KVReasonStartupExceedsCapacity
	}
	if b.KVBytesPerToken > 0 {
		perGPUBytesPerToken := b.KVBytesPerToken / float64(tp)
		if perGPUBytesPerToken > 0 {
			b.EstimatedKVTokens = int(float64(b.KVPoolBytes) / perGPUBytesPerToken)
			if b.MaxModelLen > 0 {
				perRequest := perGPUBytesPerToken * float64(b.MaxModelLen)
				n := int(float64(b.KVPoolBytes) / perRequest)
				b.MaxConcurrency = &n
			}
		}
		b.KVEstimateConfidence = b.Grade
	} else if b.Grade == GradeNotApplicable {
		// The answer is confident even though there is no pool: the model keeps
		// no KV cache is a fact, not an absence of one.
		b.KVEstimateConfidence = GradeNotApplicable
	} else if b.KVEstimateReason == "" {
		b.KVEstimateReason = KVReasonNoMeasurement
	}
	// Startup memory fitting is not enough to start: the engine refuses when
	// one request of the configured maximum context does not fit in the pool.
	if b.ContextExceedsKVPool() {
		b.Fits = false
	}
}

// ContextExceedsKVPool reports that one request of the configured maximum
// context needs more KV cache than the pool holds. An unknown KV cost, an
// unknown capacity, and a startup that already does not fit are other
// failures and keep their own reasons.
func (b Breakdown) ContextExceedsKVPool() bool {
	if b.KVBytesPerToken <= 0 || b.MaxModelLen <= 0 || b.CapacityBytes <= 0 {
		return false
	}
	if b.KVEstimateReason == KVReasonStartupExceedsCapacity {
		return false
	}
	return b.EstimatedKVTokens < b.MaxModelLen
}

// HeadCount returns the attention head count for tp divisibility (0 if unknown).
func HeadCount(cfg *modelconfig.Config) int {
	if cfg == nil {
		return 0
	}
	return cfg.NumAttentionHeads
}
