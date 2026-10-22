package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestBenchmarkVerdictAllTerminalBranches(t *testing.T) {
	tests := []struct {
		name        string
		run         BenchmarkRun
		points      []BenchmarkPoint
		status      proto.BenchmarkStatus
		kind        string
		bestSeq     int
		concurrency int
		violated    string
	}{
		{name: "grid done", run: BenchmarkRun{Mode: "grid", Search: `{"concurrency":[1,2]}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, OutputTput: 10}, {Seq: 2, Concurrency: 2, OutputTput: 18}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopGridExhausted}, kind: "grid_done", bestSeq: 2},
		{name: "saturation truncated", run: BenchmarkRun{Mode: "saturation", Search: `{"concurrency":1,"max_concurrency":2,"min_throughput_gain":0.05}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, TargetISL: 8, TargetOSL: 4, Success: 10, OutputTput: 100}, {Seq: 2, Concurrency: 2, TargetISL: 8, TargetOSL: 4, Success: 20, OutputTput: 122}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopHitMaxConcurrency}, kind: "truncated", bestSeq: 2, concurrency: 2},
		{name: "saturation found", run: BenchmarkRun{Mode: "saturation", Search: `{"concurrency":1,"max_concurrency":2,"min_throughput_gain":0.05}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, TargetISL: 8, TargetOSL: 4, Success: 10, OutputTput: 100}, {Seq: 2, Concurrency: 2, TargetISL: 8, TargetOSL: 4, Success: 20, OutputTput: 103}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopGainBelowThreshold}, kind: "saturated", bestSeq: 1, concurrency: 1},
		{name: "sla met", run: BenchmarkRun{Mode: "sla", Search: `{"concurrency":1,"max_concurrency":2,"max_p99_ttft":100,"objective":"rps"}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, TargetISL: 8, TargetOSL: 4, Success: 10, RequestTput: 5, P99TTFT: 80}, {Seq: 2, Concurrency: 2, TargetISL: 8, TargetOSL: 4, Success: 10, RequestTput: 8, P99TTFT: 120}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopSLAViolated}, kind: "sla_met", bestSeq: 1, concurrency: 1},
		{name: "sla none", run: BenchmarkRun{Mode: "sla", Search: `{"concurrency":1,"max_p99_ttft":100}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, TargetISL: 8, TargetOSL: 4, Success: 10, P99TTFT: 134}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopSLAViolated}, kind: "sla_none", bestSeq: 1, violated: "p99_ttft"},
		// 1000 / 25 ms = 40 tok/s/user misses a 45 floor even though
		// throughput ÷ concurrency (60) would have passed the old definition.
		{name: "sla interactivity", run: BenchmarkRun{Mode: "sla", Search: `{"concurrency":1,"min_interactivity":45}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, TargetISL: 8, TargetOSL: 4, Success: 10, OutputTput: 60, P50TPOT: 25}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopSLAViolated}, kind: "sla_none", bestSeq: 1, violated: "interactivity"},
		// A run stored with the retired key keeps its original judgement.
		{name: "sla legacy per-user", run: BenchmarkRun{Mode: "sla", Search: `{"concurrency":2,"min_per_user_output_tps":40}`}, points: []BenchmarkPoint{{Seq: 1, Concurrency: 2, TargetISL: 8, TargetOSL: 4, Success: 10, OutputTput: 60, P50TPOT: 5}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopSLAViolated}, kind: "sla_none", bestSeq: 1, violated: "per_user_output_tps"},
		{name: "grid open loop", run: BenchmarkRun{Mode: "grid", Search: `{"request_rates":[1,2]}`}, points: []BenchmarkPoint{{Seq: 1, RequestRate: 1, OutputTput: 10}, {Seq: 2, RequestRate: 2, OutputTput: 19}}, status: proto.BenchmarkStatus{State: "done", StopReason: proto.BenchmarkStopGridExhausted}, kind: "grid_done", bestSeq: 2},
		{name: "failed", run: BenchmarkRun{Mode: "grid"}, points: []BenchmarkPoint{{Seq: 1, OutputTput: 12}}, status: proto.BenchmarkStatus{State: "failed", StopReason: proto.BenchmarkStopFatal}, kind: "failed", bestSeq: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := computeBenchmarkVerdict(&tt.run, tt.points, tt.status)
			if !ok || got.Kind != tt.kind || got.BestPointSeq != tt.bestSeq {
				t.Fatalf("verdict=%+v ok=%v", got, ok)
			}
			if tt.concurrency != 0 {
				actual := got.SaturationConcurrency
				if tt.kind == "sla_met" {
					actual = got.SLAMaxConcurrency
				}
				if actual != tt.concurrency {
					t.Fatalf("concurrency=%d want=%d", actual, tt.concurrency)
				}
			}
			if tt.violated != "" && (len(got.Violated) != 1 || got.Violated[0].Metric != tt.violated) {
				t.Fatalf("violations=%+v", got.Violated)
			}
		})
	}
}

func TestFailedBenchmarkVerdictKeepsOnlyUsableCompletedPoints(t *testing.T) {
	for _, state := range []string{"failed", "canceled", "running"} {
		t.Run(state, func(t *testing.T) {
			points := []BenchmarkPoint{{Seq: 2, State: state, Success: 3, OutputTput: 100}}
			status := proto.BenchmarkStatus{State: "failed", StopReason: proto.BenchmarkStopFatal}
			for _, completedState := range []string{"none", "", "done"} {
				usable := append([]BenchmarkPoint(nil), points...)
				want := 0
				if completedState != "none" {
					usable = append(usable, BenchmarkPoint{Seq: 1, State: completedState, Success: 1, OutputTput: 5})
					want = 1
				}
				verdict, ok := computeBenchmarkVerdict(&BenchmarkRun{Mode: "grid"}, usable, status)
				if !ok || verdict.Kind != "failed" || verdict.BestPointSeq != want {
					t.Fatalf("invalid point became the best measured point: %+v, want best=%d", verdict, want)
				}
			}
		})
	}
}

func TestApplyBenchmarkStatusPersistsVerdictOnlyWithExplicitStopReason(t *testing.T) {
	st, err := Open("file:benchmark_verdict_persistence?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", Mode: "grid", Search: `{"concurrency":1}`})
	if err != nil {
		t.Fatal(err)
	}
	point, _ := json.Marshal(BenchmarkPoint{Seq: 1, Concurrency: 1, Success: 1, OutputTput: 10})
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: point}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "done", StopReason: proto.BenchmarkStopGridExhausted}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetBenchmarkRun(run.ID)
	var verdict BenchmarkVerdict
	if err := json.Unmarshal([]byte(got.Verdict), &verdict); err != nil || verdict.Kind != "grid_done" {
		t.Fatalf("verdict=%q parsed=%+v err=%v", got.Verdict, verdict, err)
	}

	legacy, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", Mode: "grid"})
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: legacy.ID, State: "done"}); err != nil {
		t.Fatal(err)
	}
	legacy, _ = st.GetBenchmarkRun(legacy.ID)
	if legacy.Verdict != "" {
		t.Fatalf("legacy status must not be guessed: %s", legacy.Verdict)
	}
}
