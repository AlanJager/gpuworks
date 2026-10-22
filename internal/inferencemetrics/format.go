package inferencemetrics

import (
	"fmt"
	"strings"
)

// Format is how an engine's metrics endpoint encodes its numbers.
type Format string

const (
	// FormatPrometheus is the Prometheus text exposition; the default.
	FormatPrometheus Format = "prometheus"
	// FormatJSON is one JSON document of the engine's own shape (Strata's
	// /metrics). A rule's source is the dotted path of a number in it.
	FormatJSON Format = "json"
)

// ParseFormat reads a declared format; empty is Prometheus.
func ParseFormat(declared string) (Format, bool) {
	switch Format(strings.ToLower(strings.TrimSpace(declared))) {
	case "", FormatPrometheus:
		return FormatPrometheus, true
	case FormatJSON:
		return FormatJSON, true
	}
	return "", false
}

// JSONSource is a parsed JSON rule source: the object keys leading to a
// value and, for a comparison, the text that value is tested against.
type JSONSource struct {
	Keys []string
	// Compare is empty for a number (or a boolean, read as 1/0); "==" or "!="
	// tests a string value and reads 1 when the test holds, 0 when not - the
	// way an engine that reports its state as a word ("generating", "idle")
	// still yields a gauge.
	Compare string
	Value   string
}

// ParseJSONSource reads a JSON rule source: a dotted key path such as
// totals.output_tokens, optionally followed by ==word or !=word, as in
// live.state!=idle. Keys and the word are letters, digits, "_" and "-" (the
// word may also hold "."); ok is false for anything else or an empty part.
func ParseJSONSource(source string) (JSONSource, bool) {
	source = strings.TrimSpace(source)
	var parsed JSONSource
	path := source
	for _, compare := range []string{"==", "!="} {
		if index := strings.Index(source, compare); index >= 0 {
			path, parsed.Compare, parsed.Value = source[:index], compare, source[index+len(compare):]
			if parsed.Value == "" || !jsonWord(parsed.Value, true) {
				return JSONSource{}, false
			}
			break
		}
	}
	if path == "" {
		return JSONSource{}, false
	}
	parsed.Keys = strings.Split(path, ".")
	for _, key := range parsed.Keys {
		if key == "" || !jsonWord(key, false) {
			return JSONSource{}, false
		}
	}
	return parsed, true
}

func jsonWord(word string, dots bool) bool {
	for _, c := range word {
		if !(c == '_' || c == '-' || dots && c == '.' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

// ValidateJSONRules checks rules of a JSON endpoint: every source is a dotted
// key path and every target a counter or gauge - a JSON document carries
// numbers, not histograms - and a comparison, a 0/1 reading, fills a gauge
// only.
func ValidateJSONRules(rules []Rule) error {
	seen := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Source) == "" {
			return &RuleError{Problem: ProblemEmptySource, Metric: rule.Metric}
		}
		parsed, ok := ParseJSONSource(rule.Source)
		if !ok {
			return &RuleError{Problem: ProblemBadJSONPath, Source: rule.Source, Metric: rule.Metric}
		}
		definition, known := Lookup(rule.Metric)
		if !known {
			return &RuleError{Problem: ProblemUnknownMetric, Source: rule.Source, Metric: rule.Metric}
		}
		if definition.Kind == Histogram {
			return &RuleError{Problem: ProblemJSONHistogram, Source: rule.Source, Metric: rule.Metric}
		}
		if parsed.Compare != "" && definition.Kind != Gauge {
			return &RuleError{Problem: ProblemJSONCompareCounter, Source: rule.Source, Metric: rule.Metric}
		}
		if rule.Scale < 0 {
			return &RuleError{Problem: ProblemNegativeScale, Source: rule.Source, Metric: rule.Metric}
		}
		if _, duplicate := seen[rule.Source]; duplicate {
			return &RuleError{Problem: ProblemDuplicateSource, Source: rule.Source, Metric: rule.Metric}
		}
		seen[rule.Source] = struct{}{}
	}
	return nil
}

func jsonRuleMessage(e *RuleError) (string, bool) {
	switch e.Problem {
	case ProblemBadJSONPath:
		return fmt.Sprintf("metric source %q is not a JSON key path; write keys joined by dots, like totals.output_tokens, optionally tested with ==word or !=word", e.Source), true
	case ProblemJSONHistogram:
		return fmt.Sprintf("metric source %q maps to histogram %q; a JSON endpoint can only fill counters and gauges", e.Source, e.Metric), true
	case ProblemJSONCompareCounter:
		return fmt.Sprintf("metric source %q is a comparison, read as 1 or 0; it can fill a gauge, not counter %q", e.Source, e.Metric), true
	}
	return "", false
}
