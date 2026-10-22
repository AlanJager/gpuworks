package inferencemetrics

import (
	"fmt"
	"sort"
	"strings"
)

// Rule maps one exposition family of an engine's Prometheus endpoint onto a
// platform metric.
type Rule struct {
	// Source is the exposition family name, optionally with a label selector:
	// `vllm:kv_offload_tiering_chunk_hits_total{tier="1:fs"}`. A histogram is
	// named by its bare family name; the collector expands _bucket/_sum/_count.
	Source string `json:"source"`
	// Metric is the platform metric's short name (Definition.Name).
	Metric string `json:"metric"`
	// Scale multiplies a counter or gauge value (milliseconds to seconds,
	// 0..100 to 0..1). Zero means one. A histogram cannot be scaled: its le
	// bounds would have to change with it.
	Scale float64 `json:"scale,omitempty"`
}

// RuleProblem names what is wrong with a rule. Callers turn it into their own
// stable error codes; this package has no error vocabulary of its own.
type RuleProblem string

const (
	ProblemEmptySource        RuleProblem = "empty_source"
	ProblemBadSelector        RuleProblem = "bad_selector"
	ProblemUnknownMetric      RuleProblem = "unknown_metric"
	ProblemHistogramSelector  RuleProblem = "histogram_selector"
	ProblemHistogramScale     RuleProblem = "histogram_scale"
	ProblemNegativeScale      RuleProblem = "negative_scale"
	ProblemShadowedHistogram  RuleProblem = "shadowed_histogram_suffix"
	ProblemDuplicateSource    RuleProblem = "duplicate_source"
	ProblemBadJSONPath        RuleProblem = "bad_json_path"
	ProblemJSONHistogram      RuleProblem = "json_histogram"
	ProblemJSONCompareCounter RuleProblem = "json_compare_counter"
)

// RuleError is a rule that cannot be applied.
type RuleError struct {
	Problem RuleProblem
	Source  string
	Metric  string
}

func (e *RuleError) Error() string {
	if message, ok := jsonRuleMessage(e); ok {
		return message
	}
	switch e.Problem {
	case ProblemEmptySource:
		return fmt.Sprintf("metric rule for %q has no source", e.Metric)
	case ProblemBadSelector:
		return fmt.Sprintf("metric source %q has a malformed label selector; write name{label=\"value\"}", e.Source)
	case ProblemUnknownMetric:
		return fmt.Sprintf("metric source %q maps to %q, which is not a platform inference metric", e.Source, e.Metric)
	case ProblemHistogramSelector:
		return fmt.Sprintf("metric source %q maps to histogram %q and must name the bare family, without a label selector", e.Source, e.Metric)
	case ProblemHistogramScale:
		return fmt.Sprintf("metric source %q maps to histogram %q, which cannot be scaled", e.Source, e.Metric)
	case ProblemNegativeScale:
		return fmt.Sprintf("metric source %q has a negative scale", e.Source)
	case ProblemShadowedHistogram:
		return fmt.Sprintf("metric source %q is a _bucket/_sum/_count line of a histogram family that is also mapped; map the family only", e.Source)
	case ProblemDuplicateSource:
		return fmt.Sprintf("metric source %q is mapped twice", e.Source)
	default:
		return fmt.Sprintf("metric source %q: %s", e.Source, e.Problem)
	}
}

// ValidateRules checks rules against the vocabulary and against the two ways
// a mapping silently does nothing in the collector: a label selector on a
// histogram family (the collector matches the bare family before it looks at
// selectors), and a _bucket/_sum/_count entry next to its own family (the
// family entry wins and the suffixed one is dead).
func ValidateRules(rules []Rule) error {
	seen := make(map[string]struct{}, len(rules))
	histograms := map[string]struct{}{}
	for _, rule := range rules {
		if strings.TrimSpace(rule.Source) == "" {
			return &RuleError{Problem: ProblemEmptySource, Metric: rule.Metric}
		}
		name, _, hasSelector, ok := SplitSource(rule.Source)
		if !ok {
			return &RuleError{Problem: ProblemBadSelector, Source: rule.Source, Metric: rule.Metric}
		}
		definition, known := Lookup(rule.Metric)
		if !known {
			return &RuleError{Problem: ProblemUnknownMetric, Source: rule.Source, Metric: rule.Metric}
		}
		if rule.Scale < 0 {
			return &RuleError{Problem: ProblemNegativeScale, Source: rule.Source, Metric: rule.Metric}
		}
		if definition.Kind == Histogram {
			if hasSelector {
				return &RuleError{Problem: ProblemHistogramSelector, Source: rule.Source, Metric: rule.Metric}
			}
			if rule.Scale != 0 && rule.Scale != 1 {
				return &RuleError{Problem: ProblemHistogramScale, Source: rule.Source, Metric: rule.Metric}
			}
			histograms[name] = struct{}{}
		}
		if _, duplicate := seen[rule.Source]; duplicate {
			return &RuleError{Problem: ProblemDuplicateSource, Source: rule.Source, Metric: rule.Metric}
		}
		seen[rule.Source] = struct{}{}
	}
	for _, rule := range rules {
		name, _, _, _ := SplitSource(rule.Source)
		for _, suffix := range [...]string{"_bucket", "_sum", "_count"} {
			if base := strings.TrimSuffix(name, suffix); base != name {
				if _, mapped := histograms[base]; mapped {
					return &RuleError{Problem: ProblemShadowedHistogram, Source: rule.Source, Metric: rule.Metric}
				}
			}
		}
	}
	return nil
}

// SplitSource separates a rule source into the family name and its label
// selector text (without braces). ok is false for a malformed selector.
func SplitSource(source string) (name, selector string, hasSelector, ok bool) {
	source = strings.TrimSpace(source)
	open := strings.IndexByte(source, '{')
	if open < 0 {
		return source, "", false, source != "" && !strings.ContainsAny(source, "} \t")
	}
	if !strings.HasSuffix(source, "}") || open == 0 {
		return "", "", false, false
	}
	name, selector = source[:open], source[open+1:len(source)-1]
	if strings.TrimSpace(selector) == "" || strings.ContainsAny(name, "{} \t") {
		return "", "", false, false
	}
	for _, pair := range strings.Split(selector, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(pair), "=")
		value = strings.TrimSpace(value)
		if !found || strings.TrimSpace(key) == "" || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
			return "", "", false, false
		}
	}
	return name, selector, true, true
}

// Overlay applies overlay on top of base: a rule whose Source equals one in
// base replaces it, the others are added. The result is sorted by Source, so
// the same inputs always produce the same rule list (and the same stored
// observation, which the in-place refresh compares).
func Overlay(base, overlay []Rule) []Rule {
	bySource := make(map[string]Rule, len(base)+len(overlay))
	for _, rule := range base {
		bySource[rule.Source] = rule
	}
	for _, rule := range overlay {
		bySource[rule.Source] = rule
	}
	out := make([]Rule, 0, len(bySource))
	for _, rule := range bySource {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	return out
}
