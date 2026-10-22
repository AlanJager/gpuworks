package api

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// TestBenchmarkLongTailDTOReadsOldMapRows: rows written before
// benchrun.LongTailDoc existed hold encoding/json of a map[string]any - the
// same PascalCase keys, alphabetically ordered, optional facts absent. They
// answer the same snake_case wire document as new rows.
func TestBenchmarkLongTailDTOReadsOldMapRows(t *testing.T) {
	old := `{"ApproximateTokens":true,"ITL":{"50":1},"Latency":{},"PeakInputTokens":8,"TPOT":{"99":2},"TTFT":{"50":3}}`
	raw, err := json.Marshal(benchmarkLongTailDTO(old))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ttft_ms":{"50":3},"tpot_ms":{"99":2},"latency_ms":{},"itl_ms":{"50":1},"approximate_tokens":true,"peak_input_tokens":8}`
	if string(raw) != want {
		t.Fatalf("old long_tail wire JSON\n got %s\nwant %s", raw, want)
	}

	oldWithFacts := `{"ApproximateTokens":false,"CacheReportCoverage":1,"ITL":{},"KVHitRate":0.91,"Latency":{"99":120.5},"PeakInputTokens":85800,"Speculative":{"AcceptanceRate":0.5,"TokensPerIteration":2},"TPOT":{},"TTFT":{}}`
	raw, err = json.Marshal(benchmarkLongTailDTO(oldWithFacts))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"ttft_ms":{},"tpot_ms":{},"latency_ms":{"99":120.5},"itl_ms":{},"approximate_tokens":false,"kv_hit_rate":0.91,"cache_report_coverage":1,"peak_input_tokens":85800,"speculative":{"tokens_per_iteration":2,"acceptance_rate":0.5}}`
	if string(raw) != want {
		t.Fatalf("old long_tail with facts wire JSON\n got %s\nwant %s", raw, want)
	}
}

// TestBenchmarkLongTailDTOReadsCurrentRows: a document written by
// benchrun.LongTail today answers the same wire keys.
func TestBenchmarkLongTailDTOReadsCurrentRows(t *testing.T) {
	hit := 0.25
	stored, err := json.Marshal(benchrun.LongTailDoc{
		TTFT: map[string]float64{"50": 10}, TPOT: map[string]float64{}, Latency: map[string]float64{}, ITL: map[string]float64{},
		KVHitRate: &hit, PeakInputTokens: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(benchmarkLongTailDTO(string(stored)))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"ttft_ms":{"50":10},"tpot_ms":{},"latency_ms":{},"itl_ms":{},"approximate_tokens":false,"kv_hit_rate":0.25,"peak_input_tokens":3}`
	if string(raw) != want {
		t.Fatalf("long_tail wire JSON\n got %s\nwant %s", raw, want)
	}
}

// TestBenchmarkPointDTOOmitsMissingOrBrokenLongTail: no stored document, or
// one that does not parse, leaves long_tail out of the point.
func TestBenchmarkPointDTOOmitsMissingOrBrokenLongTail(t *testing.T) {
	for _, stored := range []string{"", "not json", `{"TTFT":"fast"}`} {
		if got := benchmarkLongTailDTO(stored); got != nil {
			t.Fatalf("benchmarkLongTailDTO(%q) = %+v, want nil", stored, got)
		}
		raw, err := json.Marshal(benchmarkPointDTOFromEntity(store.BenchmarkPoint{LongTail: stored}))
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		if _, ok := keys["long_tail"]; ok {
			t.Fatalf("point with long_tail %q encodes long_tail: %s", stored, raw)
		}
	}
}

func TestBenchmarkPointEvidenceWirePreservesSeedAndUsesValidTimingGroup(t *testing.T) {
	stored := `{"Evidence":{"version":1,"execution":{"seed":"9223372036854775807"},"all":{"ttft":{"count":2,"mean_ms":21,"p50_ms":20,"p99_ms":22},"tpot":{"count":0},"last_output_latency":{"count":1,"mean_ms":300,"p50_ms":300,"p99_ms":300}}}}`
	raw, err := json.Marshal(benchmarkPointDTOFromEntity(store.BenchmarkPoint{AvgTTFT: 1, P50TTFT: 1, P99TTFT: 1, AvgTPOT: 9, P50TPOT: 9, P99TPOT: 9, LongTail: stored}))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"avg_ttft": "21", "p50_ttft": "20", "p99_ttft": "22", "p50_tpot": "0", "avg_latency": "300"} {
		if string(wire[key]) != want {
			t.Fatalf("%s=%s want=%s", key, wire[key], want)
		}
	}
	var long struct {
		Evidence struct {
			Version   int `json:"version"`
			Execution struct {
				Seed string `json:"seed"`
			} `json:"execution"`
			All struct {
				TPOT struct {
					Count int `json:"count"`
				} `json:"tpot"`
			} `json:"all"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(wire["long_tail"], &long); err != nil {
		t.Fatal(err)
	}
	if long.Evidence.Version != 1 || long.Evidence.Execution.Seed != "9223372036854775807" || long.Evidence.All.TPOT.Count != 0 {
		t.Fatalf("wire=%s", raw)
	}
}
