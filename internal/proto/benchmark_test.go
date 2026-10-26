package proto

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBenchmarkPointResultStableWireShape(t *testing.T) {
	started := time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC)
	ended := started.Add(12 * time.Second)
	raw, err := json.Marshal(BenchmarkPointResult{Seq: 2, Concurrency: 4, TargetISL: 64, TargetOSL: 16, State: "done", MaxOutputTput: 42.5, MaxTotalTput: 84.5, StartedAt: &started, EndedAt: &ended})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"seq":2`, `"concurrency":4`, `"target_isl":64`, `"target_osl":16`, `"max_output_tput":42.5`, `"max_total_tput":84.5`, `"started_at":`, `"ended_at":`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("wire %s missing %s", raw, key)
		}
	}
}

func TestBenchmarkStatusProgressWireIncludesCurrentPointAndOutcomeSample(t *testing.T) {
	sample, _ := json.Marshal(map[string]any{"success": 3, "errors": 1})
	raw, err := json.Marshal(BenchmarkStatus{ID: 1, State: "running", Completed: 4, CurrentPoint: &BenchmarkCurrentPoint{Seq: 2, Concurrency: 4, TargetISL: 64, TargetOSL: 16, RequestTotal: 8}, Sample: sample})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"current_point":`, `"request_total":8`, `"success":3`, `"errors":1`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("wire %s missing %s", raw, key)
		}
	}
}

func TestWarmupAttemptEvidenceHasSeparateStableWireField(t *testing.T) {
	const wire = `{"id":98,"state":"failed","warmup":[{"version":1,"run_id":98,"point_seq":0,"session_id":"warmup-1","attempt":2,"http_status":503,"error_stage":"http","error_text":"engine restarting"}],"message":"warmup failed"}`
	var status BenchmarkStatus
	if err := json.Unmarshal([]byte(wire), &status); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["point"]; ok {
		t.Fatal("warmup synthesized a measured point")
	}
	if _, ok := fields["result"]; ok {
		t.Fatal("warmup synthesized measured aggregate")
	}
	if string(fields["warmup"]) != string(status.Warmup) || !strings.Contains(string(raw), `"attempt":2`) {
		t.Fatalf("warmup wire=%s", raw)
	}
}
