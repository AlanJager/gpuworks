package inferencemetrics

import (
	"errors"
	"testing"
)

func TestRequestStatRulesValidate(t *testing.T) {
	ok := []RequestStatRule{
		{Path: "tensorfold.prefill_s", Stat: RequestPrefillSeconds},
		{Path: "timings.predicted_ms", Stat: RequestDecodeSeconds, Scale: 0.001},
	}
	if err := ValidateRequestStatRules(ok); err != nil {
		t.Fatalf("valid rules rejected: %v", err)
	}
	cases := map[string]struct {
		rules []RequestStatRule
		want  RuleProblem
	}{
		"bad path":       {[]RequestStatRule{{Path: "metrics..queue", Stat: RequestQueueSeconds}}, ProblemBadJSONPath},
		"selector path":  {[]RequestStatRule{{Path: "live.state!=idle", Stat: RequestQueueSeconds}}, ProblemBadJSONPath},
		"unknown stat":   {[]RequestStatRule{{Path: "metrics.queue_time_ms", Stat: "ttft_seconds"}}, ProblemUnknownMetric},
		"negative scale": {[]RequestStatRule{{Path: "metrics.queue_time_ms", Stat: RequestQueueSeconds, Scale: -1}}, ProblemNegativeScale},
		"duplicate path": {[]RequestStatRule{{Path: "a.b", Stat: RequestQueueSeconds}, {Path: "a.b", Stat: RequestDecodeSeconds}}, ProblemDuplicateSource},
	}
	for name, c := range cases {
		var ruleErr *RuleError
		if err := ValidateRequestStatRules(c.rules); !errors.As(err, &ruleErr) || ruleErr.Problem != c.want {
			t.Errorf("%s: got %v, want problem %s", name, err, c.want)
		}
	}
}

func TestEngineRequestStats(t *testing.T) {
	vllm := EngineRequestStats("VLLM ")
	if len(vllm) != 3 {
		t.Fatalf("vllm rules = %+v", vllm)
	}
	if err := ValidateRequestStatRules(vllm); err != nil {
		t.Fatalf("built-in vllm rules invalid: %v", err)
	}
	for _, rule := range vllm {
		if rule.Factor() != 0.001 {
			t.Errorf("%s: vLLM reports milliseconds, factor = %v", rule.Path, rule.Factor())
		}
	}
	vllm[0].Path = "mutated"
	if EngineRequestStats("vllm")[0].Path == "mutated" {
		t.Fatal("EngineRequestStats must return a copy")
	}
	if EngineRequestStats("sglang") != nil {
		t.Fatal("sglang replies carry no request stats while streaming")
	}
	if (RequestStatRule{}).Factor() != 1 {
		t.Fatal("zero scale means 1")
	}
}
