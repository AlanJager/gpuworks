package inferencemetrics

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseFormat(t *testing.T) {
	for declared, want := range map[string]Format{"": FormatPrometheus, "prometheus": FormatPrometheus, " JSON ": FormatJSON, "json": FormatJSON} {
		if got, ok := ParseFormat(declared); !ok || got != want {
			t.Fatalf("ParseFormat(%q) = %q %v", declared, got, ok)
		}
	}
	for _, declared := range []string{"openmetrics", "yaml", "text"} {
		if _, ok := ParseFormat(declared); ok {
			t.Fatalf("ParseFormat(%q) accepted", declared)
		}
	}
}

func TestParseJSONSource(t *testing.T) {
	for source, want := range map[string]JSONSource{
		" totals.output_tokens ": {Keys: []string{"totals", "output_tokens"}},
		"live-stats.queued_2":    {Keys: []string{"live-stats", "queued_2"}},
		"live.state!=idle":       {Keys: []string{"live", "state"}, Compare: "!=", Value: "idle"},
		"engine.kv==int8":        {Keys: []string{"engine", "kv"}, Compare: "==", Value: "int8"},
		"engine.version==0.1.38": {Keys: []string{"engine", "version"}, Compare: "==", Value: "0.1.38"},
	} {
		if got, ok := ParseJSONSource(source); !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseJSONSource(%q) = %+v %v", source, got, ok)
		}
	}
	for _, bad := range []string{"", ".a", "a.", "a..b", "a b", "a[0]", `a{x="1"}`, "a:b", "a!=", "==idle", "a==b c", "a!==b", "a==b==c", "a=b", `a=="idle"`} {
		if got, ok := ParseJSONSource(bad); ok {
			t.Fatalf("ParseJSONSource(%q) accepted: %+v", bad, got)
		}
	}
}

func TestValidateJSONRules(t *testing.T) {
	cases := []struct {
		name    string
		rules   []Rule
		problem RuleProblem
	}{
		{"bad path", []Rule{{Source: "a..b", Metric: "requests_waiting"}}, ProblemBadJSONPath},
		{"prometheus family", []Rule{{Source: "tensorfold:requests_running", Metric: "requests_running"}}, ProblemBadJSONPath},
		{"histogram target", []Rule{{Source: "timings.ttft", Metric: "ttft_seconds"}}, ProblemJSONHistogram},
		{"unknown target", []Rule{{Source: "a", Metric: "tok_s"}}, ProblemUnknownMetric},
		{"negative scale", []Rule{{Source: "a", Metric: "requests_waiting", Scale: -1}}, ProblemNegativeScale},
		{"empty", []Rule{{Source: " ", Metric: "requests_waiting"}}, ProblemEmptySource},
		{"comparison into a counter", []Rule{{Source: "live.state!=idle", Metric: "requests_success_total"}}, ProblemJSONCompareCounter},
		{"duplicate", []Rule{{Source: "a", Metric: "requests_waiting"}, {Source: "a", Metric: "requests_running"}}, ProblemDuplicateSource},
	}
	for _, tc := range cases {
		err := ValidateJSONRules(tc.rules)
		var ruleErr *RuleError
		if !errors.As(err, &ruleErr) || ruleErr.Problem != tc.problem || ruleErr.Error() == "" {
			t.Fatalf("%s: err = %v, want %s", tc.name, err, tc.problem)
		}
	}
	if err := ValidateJSONRules([]Rule{{Source: "totals.output_tokens", Metric: "generation_tokens_total"}, {Source: "live.queued", Metric: "requests_waiting", Scale: 1}, {Source: "live.state!=idle", Metric: "requests_running"}}); err != nil {
		t.Fatalf("valid rules refused: %v", err)
	}
}
