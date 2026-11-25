package engines

import (
	"strconv"
	"strings"
)

// PerformanceCost is the stable lower_snake id of one kind of performance
// tradeoff an engine launch can make: a setting that keeps the engine working
// on some hardware or model at the price of throughput or latency.
//
// The registry in this file only recognizes and grades such settings. It does
// not decide whether one is justified: the built-in catalog states that next
// to the spec (inferencecatalog.PerformanceTradeoff), and the catalog's own
// test refuses a guarded setting nobody explained.
type PerformanceCost string

const (
	// CostGraphDisabled: no graph capture or replay at all. vLLM
	// --enforce-eager, -O0, a compilation config with cudagraph_mode NONE or
	// mode 0 and no graph mode, a speculative config with enforce_eager;
	// SGLang --disable-cuda-graph.
	CostGraphDisabled PerformanceCost = "graph_disabled"
	// CostPrefixCacheDisabled: shared prompt prefixes are recomputed. vLLM
	// --no-enable-prefix-caching; SGLang --disable-radix-cache.
	CostPrefixCacheDisabled PerformanceCost = "prefix_cache_disabled"
	// CostConcurrencyCapped: few sequences decode together. vLLM
	// --max-num-seqs; SGLang --max-running-requests.
	CostConcurrencyCapped PerformanceCost = "concurrency_capped"
	// CostChunkedPrefillDisabled: a long prompt stalls every decode. vLLM
	// --no-enable-chunked-prefill; SGLang --chunked-prefill-size -1.
	CostChunkedPrefillDisabled PerformanceCost = "chunked_prefill_disabled"
	// CostOverlapScheduleDisabled: SGLang --disable-overlap-schedule.
	CostOverlapScheduleDisabled PerformanceCost = "overlap_schedule_disabled"
	// CostPrefillBatchSmall: a small prefill token budget per step. vLLM
	// --max-num-batched-tokens; SGLang --chunked-prefill-size.
	CostPrefillBatchSmall PerformanceCost = "prefill_batch_small"
	// CostKVMemoryReduced: less device memory for the KV cache. vLLM
	// --gpu-memory-utilization; SGLang --mem-fraction-static.
	CostKVMemoryReduced PerformanceCost = "kv_memory_reduced"
	// CostGraphCaptureNarrow: graphs cover only part of the work. vLLM
	// cudagraph_mode FULL_DECODE_ONLY or capture sizes below 16; SGLang
	// --cuda-graph-max-bs or --cuda-graph-max-bs-decode below 16, and
	// --cuda-graph-backend-prefill disabled (decode graphs only).
	CostGraphCaptureNarrow PerformanceCost = "graph_capture_narrow"
	// CostKernelFallback: a fast kernel path is switched off (vLLM
	// VLLM_USE_DEEP_GEMM=0 and similar, the Triton attention backend).
	CostKernelFallback PerformanceCost = "kernel_fallback"
)

// PerformanceLevel grades a finding.
type PerformanceLevel string

const (
	// LevelGuarded findings must carry a stated reason in the built-in
	// catalog.
	LevelGuarded PerformanceLevel = "guarded"
	// LevelInfo findings are shown, never refused.
	LevelInfo PerformanceLevel = "info"
)

// Thresholds of the value-graded costs. A setting at or below
// guardedConcurrency is guarded; up to infoConcurrency it is information.
const (
	guardedConcurrency    = 4
	infoConcurrency       = 8
	smallPrefillTokens    = 2048
	narrowCaptureSize     = 16
	lowGPUMemoryFraction  = 0.8
	lowStaticMemFraction  = 0.7
	speculativeConfigFlag = "--speculative-config"
	compilationConfigFlag = "--compilation-config"
)

// PerformanceCosts lists every registered cost in a stable order.
func PerformanceCosts() []PerformanceCost {
	return []PerformanceCost{
		CostGraphDisabled, CostPrefixCacheDisabled, CostConcurrencyCapped,
		CostChunkedPrefillDisabled, CostOverlapScheduleDisabled, CostPrefillBatchSmall,
		CostKVMemoryReduced, CostGraphCaptureNarrow, CostKernelFallback,
	}
}

// PoolingExempt reports whether the cost is no tradeoff for a pooling task
// (embeddings, rerank): one forward pass per request and no decode, so graph
// replay, prefix reuse and decode concurrency buy little.
func (c PerformanceCost) PoolingExempt() bool {
	switch c {
	case CostGraphDisabled, CostPrefixCacheDisabled, CostConcurrencyCapped:
		return true
	default:
		return false
	}
}

// PerformanceFinding is one tradeoff found in a launch.
type PerformanceFinding struct {
	Cost  PerformanceCost
	Level PerformanceLevel
	// Param names the setting as the catalog writes it: a flag
	// ("--enforce-eager"), a key inside a JSON-valued flag
	// ("--compilation-config.cudagraph_mode"), or an environment variable
	// ("env:VLLM_USE_DEEP_GEMM").
	Param string
	// Value is the value as written; empty for a switch.
	Value string
}

// ScanPerformanceCosts reports the tradeoffs in one engine launch: argv (the
// engine arguments, flags written either "--flag value" or "--flag=value",
// and for vLLM, whose parser also accepts underscores, --max_num_seqs; the
// finding names the dashed spelling)
// and the container environment. Plain flags come in argv order, then the
// findings of vLLM's JSON-valued compilation and speculative configs (each
// merged across its JSON and dotted spellings, later values winning, as vLLM
// does), then environment keys in sorted order. An engine without a registry
// entry (Callable, an unknown name) has no findings.
func ScanPerformanceCosts(engine string, argv []string, env map[string]string) []PerformanceFinding {
	switch engine {
	case VLLM, VLLMOmni:
		return scanVLLM(argv, env)
	case SGLang:
		var findings []PerformanceFinding
		for i := 0; i < len(argv); i++ {
			findings = append(findings, scanFlag(sglangFlagRules, false, argv, &i)...)
		}
		return findings
	default:
		return nil
	}
}

// scanFlag grades argv[i] by rules, consuming the flag's value when the rule
// takes one, and returns nothing for a token no rule names.
// With underscores, an underscore spelling of a flag counts as the dashed one.
func scanFlag(rules map[string]flagRule, underscores bool, argv []string, i *int) []PerformanceFinding {
	token := strings.TrimSpace(argv[*i])
	if !strings.HasPrefix(token, "-") {
		return nil
	}
	name, inline, hasInline := strings.Cut(token, "=")
	if underscores {
		name = strings.ReplaceAll(name, "_", "-")
	}
	rule, ok := rules[name]
	if !ok {
		return nil
	}
	if !rule.takesValue {
		if hasInline && isFalse(inline) {
			return nil
		}
		return rule.scan(name, "")
	}
	value := inline
	if !hasInline {
		value = takeValue(argv, i)
	}
	return rule.scan(name, value)
}

// takeValue consumes and returns the token after argv[*i] when it is a value
// rather than the next flag. A negative number is a value.
func takeValue(argv []string, i *int) string {
	if *i+1 >= len(argv) {
		return ""
	}
	next := strings.TrimSpace(argv[*i+1])
	if strings.HasPrefix(next, "--") {
		return ""
	}
	if strings.HasPrefix(next, "-") {
		if _, err := strconv.ParseFloat(next, 64); err != nil {
			return ""
		}
	}
	*i++
	return next
}

type flagRule struct {
	takesValue bool
	scan       func(name, value string) []PerformanceFinding
}

func switchRule(cost PerformanceCost) flagRule {
	return flagRule{scan: func(name, _ string) []PerformanceFinding {
		return []PerformanceFinding{{Cost: cost, Level: LevelGuarded, Param: name}}
	}}
}

func concurrencyRule() flagRule {
	return flagRule{takesValue: true, scan: func(name, value string) []PerformanceFinding {
		n, err := strconv.Atoi(value)
		switch {
		case err != nil || n <= 0:
			return nil
		case n <= guardedConcurrency:
			return []PerformanceFinding{{Cost: CostConcurrencyCapped, Level: LevelGuarded, Param: name, Value: value}}
		case n <= infoConcurrency:
			return []PerformanceFinding{{Cost: CostConcurrencyCapped, Level: LevelInfo, Param: name, Value: value}}
		}
		return nil
	}}
}

func belowRule(cost PerformanceCost, limit float64) flagRule {
	return flagRule{takesValue: true, scan: func(name, value string) []PerformanceFinding {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || n >= limit {
			return nil
		}
		return []PerformanceFinding{{Cost: cost, Level: LevelInfo, Param: name, Value: value}}
	}}
}

var vllmFlagRules = map[string]flagRule{
	"--enforce-eager":             switchRule(CostGraphDisabled),
	"--no-enable-prefix-caching":  switchRule(CostPrefixCacheDisabled),
	"--no-enable-chunked-prefill": switchRule(CostChunkedPrefillDisabled),
	"--max-num-seqs":              concurrencyRule(),
	"--max-num-batched-tokens":    belowRule(CostPrefillBatchSmall, smallPrefillTokens),
	"--gpu-memory-utilization":    belowRule(CostKVMemoryReduced, lowGPUMemoryFraction),
	"--attention-backend":         attentionBackendRule(),
}

var sglangFlagRules = map[string]flagRule{
	"--disable-cuda-graph":       switchRule(CostGraphDisabled),
	"--disable-radix-cache":      switchRule(CostPrefixCacheDisabled),
	"--disable-overlap-schedule": switchRule(CostOverlapScheduleDisabled),
	"--max-running-requests":     concurrencyRule(),
	"--mem-fraction-static":      belowRule(CostKVMemoryReduced, lowStaticMemFraction),
	"--cuda-graph-max-bs":        belowRule(CostGraphCaptureNarrow, narrowCaptureSize),
	// SGLang 0.5.21 states graph capture per phase. A decode cap below 16
	// narrows capture as --cuda-graph-max-bs does; a prefill backend of
	// "disabled" leaves decode graphs only, as vLLM's FULL_DECODE_ONLY does.
	"--cuda-graph-max-bs-decode": belowRule(CostGraphCaptureNarrow, narrowCaptureSize),
	"--cuda-graph-backend-prefill": {takesValue: true, scan: func(name, value string) []PerformanceFinding {
		if strings.EqualFold(value, "disabled") {
			return []PerformanceFinding{{Cost: CostGraphCaptureNarrow, Level: LevelInfo, Param: name, Value: value}}
		}
		return nil
	}},
	"--chunked-prefill-size": {takesValue: true, scan: func(name, value string) []PerformanceFinding {
		if n, err := strconv.Atoi(value); err == nil && n < 0 {
			return []PerformanceFinding{{Cost: CostChunkedPrefillDisabled, Level: LevelGuarded, Param: name, Value: value}}
		}
		return belowRule(CostPrefillBatchSmall, smallPrefillTokens).scan(name, value)
	}},
}

func attentionBackendRule() flagRule {
	return flagRule{takesValue: true, scan: func(name, value string) []PerformanceFinding {
		if strings.EqualFold(value, "TRITON_ATTN") {
			return []PerformanceFinding{{Cost: CostKernelFallback, Level: LevelInfo, Param: name, Value: value}}
		}
		return nil
	}}
}

func isFalse(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

func isTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
