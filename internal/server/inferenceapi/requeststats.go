package inferenceapi

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/messages"
)

// api.request_stats is a template's map from fields its engine writes into
// each reply to the platform's request stats: TensorFold's
// tensorfold.prefill_s, llama.cpp's timings.prompt_ms. The gateway reads them
// as a reply ends (docs/reference/inference-catalog-spec.md#apirequest_stats).
// Engines the platform knows bring their own (inferencemetrics.EngineRequestStats);
// a template's entry wins over the engine's for the same path.

// RequestStatRules is the declaration as rules, sorted by path.
func RequestStatRules(stats MetricMap) []inferencemetrics.RequestStatRule {
	rules := make([]inferencemetrics.RequestStatRule, 0, len(stats))
	for path, target := range stats {
		rules = append(rules, inferencemetrics.RequestStatRule{Path: path, Stat: target.To, Scale: target.Scale})
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].Path < rules[j].Path })
	return rules
}

// ResolveRequestStats is the rules the gateway applies to a deployment's
// replies: the engine's built-in rules overlaid with the template's.
func ResolveRequestStats(engine string, declared MetricMap) []inferencemetrics.RequestStatRule {
	byPath := map[string]inferencemetrics.RequestStatRule{}
	for _, rule := range inferencemetrics.EngineRequestStats(engine) {
		byPath[rule.Path] = rule
	}
	for _, rule := range RequestStatRules(declared) {
		byPath[rule.Path] = rule
	}
	out := make([]inferencemetrics.RequestStatRule, 0, len(byPath))
	for _, rule := range byPath {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func normalizeRequestStats(stats MetricMap) (MetricMap, error) {
	trimmed := make(MetricMap, len(stats))
	for path, target := range stats {
		path = strings.TrimSpace(path)
		target.To = strings.TrimSpace(target.To)
		if _, duplicate := trimmed[path]; duplicate {
			return nil, requestStatError(&inferencemetrics.RuleError{Problem: inferencemetrics.ProblemDuplicateSource, Source: path, Metric: target.To})
		}
		trimmed[path] = target
	}
	if err := inferencemetrics.ValidateRequestStatRules(RequestStatRules(trimmed)); err != nil {
		return nil, requestStatError(err)
	}
	return trimmed, nil
}

var requestStatCodes = map[inferencemetrics.RuleProblem]string{
	inferencemetrics.ProblemBadJSONPath:     messages.ErrorCatalogRequestStatsPathInvalid,
	inferencemetrics.ProblemUnknownMetric:   messages.ErrorCatalogRequestStatsUnknownStat,
	inferencemetrics.ProblemNegativeScale:   messages.ErrorCatalogRequestStatsNegativeScale,
	inferencemetrics.ProblemDuplicateSource: messages.ErrorCatalogRequestStatsDuplicatePath,
}

func requestStatError(err error) error {
	var ruleErr *inferencemetrics.RuleError
	if !errors.As(err, &ruleErr) {
		return err
	}
	code, ok := requestStatCodes[ruleErr.Problem]
	if !ok {
		return err
	}
	return messages.NewProductError(code,
		fmt.Sprintf("api.request_stats entry %q -> %q: %s", ruleErr.Source, ruleErr.Metric, ruleErr.Problem),
		map[string]any{"path": ruleErr.Source, "stat": ruleErr.Metric})
}
