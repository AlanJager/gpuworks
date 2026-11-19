package inferencecatalog

import (
	"fmt"
	"net/url"
	"strings"

	"gpuworks.ai/gpuworks/internal/engines"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// PerformanceTradeoffReason is why a spec keeps a setting the engine registry
// grades as a performance cost (engines.ScanPerformanceCosts). It is catalog
// vocabulary, lower_snake like the cost ids.
type PerformanceTradeoffReason string

const (
	// ReasonHardwarePluginRequires: the vendor plugin, runtime or driver does
	// not support the faster path (graph capture, prefix caching, a kernel).
	ReasonHardwarePluginRequires PerformanceTradeoffReason = "hardware_plugin_requires"
	// ReasonModelArchitectureRequires: the model's structure is unsupported on
	// the faster path in this engine version.
	ReasonModelArchitectureRequires PerformanceTradeoffReason = "model_architecture_requires"
	// ReasonAccuracy: the faster path produces wrong or degraded output.
	ReasonAccuracy PerformanceTradeoffReason = "accuracy"
	// ReasonMemoryFit: memory or concurrency is given up so the model fits
	// the card or slice.
	ReasonMemoryFit PerformanceTradeoffReason = "memory_fit"
	// ReasonTaskType: a task without decode (embeddings, rerank). Pooling
	// specs get it without writing it (PoolingOnly).
	ReasonTaskType PerformanceTradeoffReason = "task_type"
	// ReasonUpstreamBug: a known upstream defect; Link is required.
	ReasonUpstreamBug PerformanceTradeoffReason = "upstream_bug"
)

// PerformanceTradeoffReasons lists every reason in a stable order.
func PerformanceTradeoffReasons() []PerformanceTradeoffReason {
	return []PerformanceTradeoffReason{
		ReasonHardwarePluginRequires, ReasonModelArchitectureRequires, ReasonAccuracy,
		ReasonMemoryFit, ReasonTaskType, ReasonUpstreamBug,
	}
}

// PerformanceTradeoff is a spec's stated reason for one setting that costs
// performance. Param names the setting exactly as the registry reports it
// (engines.PerformanceFinding.Param), and covers every occurrence of it in the
// spec's launch. DetailEN and DetailZH are catalog content, like display
// names, written by the spec's author.
type PerformanceTradeoff struct {
	Param    string                    `yaml:"param" json:"param"`
	Reason   PerformanceTradeoffReason `yaml:"reason" json:"reason"`
	DetailEN string                    `yaml:"detail_en" json:"detail_en"`
	DetailZH string                    `yaml:"detail_zh" json:"detail_zh"`
	// Link points at the upstream issue or recipe; required for upstream_bug.
	Link string `yaml:"link,omitempty" json:"link,omitempty"`
}

// PerformanceFinding is a registry finding together with the part of the spec
// it was found in: "args", "env", "entrypoint", "pre_start_argv" or
// "manual_command".
type PerformanceFinding struct {
	engines.PerformanceFinding
	Where string
}

// SpecPerformanceFindings grades every launch input the spec states: its
// engine args and environment, and the flags written inside its entrypoint,
// pre-start argv and manual command (split on whitespace, so a JSON value
// with spaces inside a shell string is not read).
func SpecPerformanceFindings(spec InferenceSpec) []PerformanceFinding {
	var findings []PerformanceFinding
	add := func(where string, found []engines.PerformanceFinding) {
		for _, finding := range found {
			findings = append(findings, PerformanceFinding{PerformanceFinding: finding, Where: where})
		}
	}
	add("args", engines.ScanPerformanceCosts(spec.Engine, spec.Args, nil))
	add("env", engines.ScanPerformanceCosts(spec.Engine, nil, spec.Env))
	add("entrypoint", engines.ScanPerformanceCosts(spec.Engine, shellWords(spec.Entrypoint...), nil))
	add("pre_start_argv", engines.ScanPerformanceCosts(spec.Engine, shellWords(spec.PreStartArgv...), nil))
	add("manual_command", engines.ScanPerformanceCosts(spec.Engine, shellWords(spec.ManualCommand), nil))
	return findings
}

func shellWords(values ...string) []string {
	var words []string
	for _, value := range values {
		for _, word := range strings.Fields(value) {
			words = append(words, strings.Trim(word, `'"`))
		}
	}
	return words
}

// PoolingOnly reports whether every operation the spec serves is a pooling
// one (embeddings, rerank): such a spec has stated reason task_type for every
// engines.PerformanceCost.PoolingExempt finding.
func PoolingOnly(spec InferenceSpec) bool {
	if spec.API == nil || len(spec.API.Operations) == 0 {
		return false
	}
	for _, op := range spec.API.Operations {
		if op.Type != inferenceapi.OperationEmbeddings && op.Type != inferenceapi.OperationRerank {
			return false
		}
	}
	return true
}

// UnexplainedPerformanceFindings returns the guarded findings of the spec that
// neither a PerformanceTradeoff states nor PoolingOnly exempts.
func UnexplainedPerformanceFindings(spec InferenceSpec) []PerformanceFinding {
	stated := make(map[string]struct{}, len(spec.PerformanceTradeoffs))
	for _, tradeoff := range spec.PerformanceTradeoffs {
		stated[tradeoff.Param] = struct{}{}
	}
	pooling := PoolingOnly(spec)
	var unexplained []PerformanceFinding
	for _, finding := range SpecPerformanceFindings(spec) {
		if finding.Level != engines.LevelGuarded {
			continue
		}
		if _, ok := stated[finding.Param]; ok {
			continue
		}
		if pooling && finding.Cost.PoolingExempt() {
			continue
		}
		unexplained = append(unexplained, finding)
	}
	return unexplained
}

// validatePerformanceTradeoffs checks each stated tradeoff: a known reason,
// both details, a link where the reason needs one, one entry per param, and a
// param the registry still finds in the spec — a reason must not outlive its
// setting.
func validatePerformanceTradeoffs(modelID string, spec *InferenceSpec) error {
	if len(spec.PerformanceTradeoffs) == 0 {
		return nil
	}
	found := make(map[string]struct{})
	for _, finding := range SpecPerformanceFindings(*spec) {
		found[finding.Param] = struct{}{}
	}
	seen := make(map[string]struct{}, len(spec.PerformanceTradeoffs))
	for i := range spec.PerformanceTradeoffs {
		tradeoff := &spec.PerformanceTradeoffs[i]
		tradeoff.Param = strings.TrimSpace(tradeoff.Param)
		tradeoff.Reason = PerformanceTradeoffReason(strings.TrimSpace(string(tradeoff.Reason)))
		tradeoff.DetailEN = strings.TrimSpace(tradeoff.DetailEN)
		tradeoff.DetailZH = strings.TrimSpace(tradeoff.DetailZH)
		tradeoff.Link = strings.TrimSpace(tradeoff.Link)
		where := fmt.Sprintf("model %s spec %s performance_tradeoffs[%d]", modelID, spec.Name, i)
		if tradeoff.Param == "" {
			return fmt.Errorf("%s requires param", where)
		}
		if _, duplicate := seen[tradeoff.Param]; duplicate {
			return fmt.Errorf("%s repeats param %s", where, tradeoff.Param)
		}
		seen[tradeoff.Param] = struct{}{}
		if _, ok := found[tradeoff.Param]; !ok {
			return fmt.Errorf("%s states %s, which the spec's launch does not set as a performance cost", where, tradeoff.Param)
		}
		if !knownTradeoffReason(tradeoff.Reason) {
			return fmt.Errorf("%s has unknown reason %q", where, tradeoff.Reason)
		}
		if tradeoff.DetailEN == "" || tradeoff.DetailZH == "" {
			return fmt.Errorf("%s requires detail_en and detail_zh", where)
		}
		if tradeoff.Reason == ReasonUpstreamBug && tradeoff.Link == "" {
			return fmt.Errorf("%s with reason upstream_bug requires link", where)
		}
		if tradeoff.Link != "" {
			parsed, err := url.Parse(tradeoff.Link)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
				return fmt.Errorf("%s link must be an http(s) URL", where)
			}
		}
	}
	return nil
}

func knownTradeoffReason(reason PerformanceTradeoffReason) bool {
	for _, known := range PerformanceTradeoffReasons() {
		if reason == known {
			return true
		}
	}
	return false
}

// performanceTradeoffsSchema is the editor schema of a spec's
// performance_tradeoffs (PerformanceTradeoff).
func performanceTradeoffsSchema() map[string]any {
	reasons := make([]any, 0, len(PerformanceTradeoffReasons()))
	for _, reason := range PerformanceTradeoffReasons() {
		reasons = append(reasons, string(reason))
	}
	return map[string]any{
		"type":        "array",
		"description": "Why the spec keeps settings the engine registry grades as performance costs (such as --enforce-eager). Each param must be one the spec's launch sets.",
		"items": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"param", "reason", "detail_en", "detail_zh"},
			"properties": map[string]any{
				"param":     map[string]any{"type": "string", "minLength": 1, "description": "The setting as the registry names it: a flag (--enforce-eager), a JSON key (--compilation-config.cudagraph_mode) or env:NAME."},
				"reason":    map[string]any{"type": "string", "enum": reasons},
				"detail_en": map[string]any{"type": "string", "minLength": 1},
				"detail_zh": map[string]any{"type": "string", "minLength": 1},
				"link":      map[string]any{"type": "string", "format": "uri", "description": "Upstream issue or recipe; required when reason is upstream_bug."},
			},
		},
	}
}
