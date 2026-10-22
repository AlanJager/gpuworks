package inferencemetrics

import (
	"errors"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
)

func TestVocabularyNamesAreUniqueAndStoredUnderOnePrefix(t *testing.T) {
	seen := map[string]bool{}
	for _, definition := range Definitions() {
		if seen[definition.Name] {
			t.Fatalf("%s is defined twice", definition.Name)
		}
		seen[definition.Name] = true
		if strings.HasPrefix(definition.Name, SeriesPrefix) {
			t.Fatalf("%s carries the series prefix in its short name", definition.Name)
		}
		if got := definition.Series(); got != "gpuworks_infer_"+definition.Name {
			t.Fatalf("%s series = %q", definition.Name, got)
		}
		switch definition.Kind {
		case Counter, Gauge, Histogram:
		default:
			t.Fatalf("%s has kind %q", definition.Name, definition.Kind)
		}
		if definition.Unit == "" {
			t.Fatalf("%s states no unit", definition.Name)
		}
		if definition.Kind == Counter && !strings.HasSuffix(definition.Name, "_total") {
			// The collector sums duplicate counter lines by the exposition
			// name's _total suffix; a stored counter keeps the same convention.
			t.Fatalf("counter %s does not end in _total", definition.Name)
		}
	}
	if len(Names()) != len(seen) {
		t.Fatalf("Names() = %d, definitions = %d", len(Names()), len(seen))
	}
}

// The built-in mappings obey the same rules a template's mapping is held to:
// every target exists, histograms are mapped by their bare family, nothing is
// shadowed.
func TestBuiltInEngineRulesAreValid(t *testing.T) {
	for _, engine := range []string{engines.VLLM, engines.SGLang} {
		rules := EngineRules(engine)
		if len(rules) < 10 {
			t.Fatalf("%s has only %d rules", engine, len(rules))
		}
		if err := ValidateRules(rules); err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		if !HasEngineRules(engine) {
			t.Fatalf("HasEngineRules(%s) = false", engine)
		}
		for _, rule := range rules {
			if !strings.HasPrefix(rule.Source, engine+":") {
				t.Fatalf("%s rule %q is outside the %s: family prefix detection relies on", engine, rule.Source, engine)
			}
		}
	}
	if EngineRules(engines.Callable) != nil || HasEngineRules(engines.Callable) {
		t.Fatal("callable has no exposition of its own to know")
	}
	// EngineRules hands out a copy: a caller cannot edit the definition.
	rules := EngineRules(engines.VLLM)
	rules[0].Metric = "mutated"
	if EngineRules(engines.VLLM)[0].Metric == "mutated" {
		t.Fatal("EngineRules exposes its backing array")
	}
}

func TestDetectSetsCoverEveryEngineWithRules(t *testing.T) {
	sets := DetectSets()
	if len(sets) != 2 || sets[0].Prefix != engines.VLLM || sets[1].Prefix != engines.SGLang {
		t.Fatalf("detect sets = %+v", sets)
	}
	for _, set := range sets {
		if len(set.Rules) != len(EngineRules(set.Prefix)) {
			t.Fatalf("%s detect set has %d rules, engine %d", set.Prefix, len(set.Rules), len(EngineRules(set.Prefix)))
		}
	}
}

func TestValidateRulesRejectsWhatTheCollectorCannotHonour(t *testing.T) {
	cases := []struct {
		name    string
		rules   []Rule
		problem RuleProblem
	}{
		{"empty source", []Rule{{Source: " ", Metric: "requests_running"}}, ProblemEmptySource},
		{"unclosed selector", []Rule{{Source: `x{tier="1"`, Metric: "requests_running"}}, ProblemBadSelector},
		{"unquoted selector value", []Rule{{Source: `x{tier=1}`, Metric: "requests_running"}}, ProblemBadSelector},
		{"empty selector", []Rule{{Source: `x{}`, Metric: "requests_running"}}, ProblemBadSelector},
		{"space in name", []Rule{{Source: `x y`, Metric: "requests_running"}}, ProblemBadSelector},
		{"unknown target", []Rule{{Source: "x", Metric: "ttft_second"}}, ProblemUnknownMetric},
		{"stored name instead of short name", []Rule{{Source: "x", Metric: "gpuworks_infer_ttft_seconds"}}, ProblemUnknownMetric},
		{"histogram with selector", []Rule{{Source: `x{model="m"}`, Metric: "ttft_seconds"}}, ProblemHistogramSelector},
		{"histogram scaled", []Rule{{Source: "x", Metric: "ttft_seconds", Scale: 0.001}}, ProblemHistogramScale},
		{"negative scale", []Rule{{Source: "x", Metric: "requests_running", Scale: -1}}, ProblemNegativeScale},
		{"suffix next to its family", []Rule{{Source: "x", Metric: "ttft_seconds"}, {Source: "x_count", Metric: "requests_success_total"}}, ProblemShadowedHistogram},
		{"duplicate source", []Rule{{Source: "x", Metric: "requests_running"}, {Source: "x", Metric: "requests_waiting"}}, ProblemDuplicateSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRules(tc.rules)
			var ruleErr *RuleError
			if !errors.As(err, &ruleErr) || ruleErr.Problem != tc.problem {
				t.Fatalf("err = %v, want problem %s", err, tc.problem)
			}
			if ruleErr.Error() == "" {
				t.Fatal("empty message")
			}
		})
	}
	valid := []Rule{
		{Source: `x{tier="1:fs", kind="a"}`, Metric: "kv_disk_hits_total"},
		{Source: "y", Metric: "ttft_seconds", Scale: 1},
		{Source: "z_ms_total", Metric: "prompt_tokens_total", Scale: 0.001},
		{Source: "w_count", Metric: "requests_success_total"}, // no w family mapped
	}
	if err := ValidateRules(valid); err != nil {
		t.Fatalf("valid rules refused: %v", err)
	}
}

func TestOverlayLetsTheTemplateWinAndSortsBySource(t *testing.T) {
	base := []Rule{{Source: "b", Metric: "requests_waiting"}, {Source: "a", Metric: "requests_running"}}
	overlay := []Rule{{Source: "b", Metric: "requests_running"}, {Source: "c", Metric: "ttft_seconds"}}
	got := Overlay(base, overlay)
	want := []Rule{{Source: "a", Metric: "requests_running"}, {Source: "b", Metric: "requests_running"}, {Source: "c", Metric: "ttft_seconds"}}
	if len(got) != len(want) {
		t.Fatalf("overlay = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("overlay[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if len(Overlay(nil, nil)) != 0 {
		t.Fatal("empty overlay is not empty")
	}
}

func TestDeriveFillsKVUsageOnlyWhenTheEngineDidNot(t *testing.T) {
	usage := SeriesPrefix + "gpu_cache_usage_percent"
	capacity, free := SeriesPrefix+"kv_capacity_tokens", SeriesPrefix+"kv_free_tokens"
	got := Derive(map[string]float64{capacity: 1411072, free: 1396736})
	if value, ok := got[usage]; !ok || value < 0.0101 || value > 0.0102 {
		t.Fatalf("derived usage = %v (%v)", value, got)
	}
	if got := Derive(map[string]float64{capacity: 100, free: 0, usage: 0.5}); len(got) != 0 {
		t.Fatalf("a reported usage was overwritten: %v", got)
	}
	for _, values := range []map[string]float64{
		{capacity: 100},             // free missing
		{capacity: 0, free: 0},      // empty pool
		{capacity: 100, free: 200},  // inconsistent
		{capacity: 100, free: -1},   // negative
		{SeriesPrefix + "other": 1}, // nothing related
	} {
		if got := Derive(values); len(got) != 0 {
			t.Fatalf("Derive(%v) = %v, want nothing", values, got)
		}
	}
}

// The vocabulary reference template authors read lists exactly the metrics
// defined here, with the same kind and unit.
func TestVocabularyReferenceMatchesTheDefinitions(t *testing.T) {
	documented := documentedMetrics(t, "## 指标")
	for _, definition := range Definitions() {
		want := string(definition.Kind) + "/" + definition.Unit
		if got, ok := documented[definition.Name]; !ok || got != want {
			t.Errorf("%s documented as %q, defined as %q", definition.Name, got, want)
		}
		delete(documented, definition.Name)
	}
	for name := range documented {
		t.Errorf("%s is documented but not defined", name)
	}
	gateway := documentedMetrics(t, "## 网关指标")
	for _, definition := range GatewayDefinitions() {
		want := string(definition.Kind) + "/" + definition.Unit
		if got, ok := gateway[definition.Name]; !ok || got != want {
			t.Errorf("gateway %s documented as %q, defined as %q", definition.Name, got, want)
		}
		delete(gateway, definition.Name)
	}
	for name := range gateway {
		t.Errorf("gateway %s is documented but not defined", name)
	}
}

// documentedMetrics reads the metric table under a heading of the vocabulary
// reference: name -> "kind/unit".
func documentedMetrics(t *testing.T, heading string) map[string]string {
	t.Helper()
	data, err := os.ReadFile("../../docs/reference/inference-metrics.md")
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{}
	inTable := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "## ") {
			inTable = strings.TrimSpace(line) == heading
			continue
		}
		if !inTable || !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 5 {
			t.Fatalf("malformed row %q", line)
		}
		name := strings.Trim(strings.TrimSpace(cells[1]), "`")
		documented[name] = strings.TrimSpace(cells[2]) + "/" + strings.TrimSpace(cells[3])
	}
	if len(documented) == 0 {
		t.Fatalf("no metric table under %q", heading)
	}
	return documented
}

func TestGatewayVocabulary(t *testing.T) {
	seen := map[string]bool{}
	for _, definition := range GatewayDefinitions() {
		if seen[definition.Name] {
			t.Fatalf("%s defined twice", definition.Name)
		}
		seen[definition.Name] = true
		if definition.Series() != "gpuworks_proxy_"+definition.Name {
			t.Fatalf("%s series = %s", definition.Name, definition.Series())
		}
		if definition.Kind == Counter && !strings.HasSuffix(definition.Name, "_total") {
			t.Fatalf("counter %s does not end in _total", definition.Name)
		}
		if definition.Unit == "" {
			t.Fatalf("%s has no unit", definition.Name)
		}
		if Gateway(definition.Name) != definition {
			t.Fatalf("Gateway(%s) differs", definition.Name)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an unknown gateway metric did not panic")
		}
	}()
	Gateway("ttft_seconds")
}
