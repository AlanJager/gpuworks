package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestBenchmarkEvidenceGridVerdictPersistsSharedEligibility(t *testing.T) {
	// Literal stored JSON checks the real long_tail reader, rather than letting
	// a producer and consumer share a misspelled JSON field in test fixtures.
	const complete = `{"Evidence":{"version":1,"plan":{"planned_sessions":4,"planned_requests":52},"outcomes":{"completed_sessions":4},"all":{"attempted":52,"success":52,"ttft":{"count":52},"last_output_latency":{"count":52}}}}`
	const incomplete = `{"Evidence":{"version":1,"plan":{"planned_sessions":4,"planned_requests":52},"outcomes":{"completed_sessions":3,"failed_sessions":1,"skipped":12},"all":{"attempted":40,"success":39,"failed":1,"ttft":{"count":39},"last_output_latency":{"count":39}}}}`
	const noTTFT = `{"Evidence":{"version":1,"plan":{"planned_sessions":1,"planned_requests":1},"outcomes":{"completed_sessions":1},"all":{"attempted":1,"success":1,"ttft":{"count":0},"last_output_latency":{"count":1}}}}`
	tests := []struct {
		name   string
		search string
		points []BenchmarkPoint
		best   int
	}{
		{
			name: "session completion floor excludes faster incomplete point", search: `{"concurrency":[2,4],"min_session_completion":0.95}`,
			points: []BenchmarkPoint{
				{Seq: 1, Concurrency: 2, State: "done", Success: 52, OutputTput: 18, LongTail: complete},
				{Seq: 2, Concurrency: 4, State: "done", Success: 39, Errors: 1, OutputTput: 30, LongTail: incomplete},
			}, best: 1,
		},
		{
			name: "repeated coordinates retain selected measurement identity", search: `{"concurrency":[1,1],"min_session_completion":0.95}`,
			points: []BenchmarkPoint{
				{Seq: 1, Concurrency: 1, State: "done", Success: 39, Errors: 1, OutputTput: 10, LongTail: incomplete},
				{Seq: 2, Concurrency: 1, State: "done", Success: 52, OutputTput: 20, LongTail: complete},
			}, best: 2,
		},
		{
			name: "no recommendation if all completion evidence fails or is unknown", search: `{"concurrency":[2,4],"min_session_completion":0.95}`,
			points: []BenchmarkPoint{
				{Seq: 1, Concurrency: 2, State: "done", Success: 39, Errors: 1, OutputTput: 18, LongTail: incomplete},
				{Seq: 2, Concurrency: 4, State: "done", Success: 52, OutputTput: 30},
			}, best: 0,
		},
		{
			name: "explicit no samples cannot satisfy TTFT by zero", search: `{"concurrency":[1],"max_p99_ttft":100}`,
			points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, State: "done", Success: 1, OutputTput: 30, LongTail: noTTFT}}, best: 0,
		},
		{
			name: "explicit no samples override nonzero stale timing", search: `{"concurrency":[1],"max_p99_ttft":100}`,
			points: []BenchmarkPoint{{Seq: 1, Concurrency: 1, State: "done", Success: 1, OutputTput: 30, P99TTFT: 80, LongTail: noTTFT}}, best: 0,
		},
		{
			name: "old nonzero timing remains usable but old zero is unknown", search: `{"concurrency":[1,2],"max_p99_ttft":100}`,
			points: []BenchmarkPoint{
				{Seq: 1, Concurrency: 1, State: "done", Success: 1, OutputTput: 10, P99TTFT: 80},
				{Seq: 2, Concurrency: 2, State: "done", Success: 1, OutputTput: 30},
			}, best: 1,
		},
		{
			name: "failed and canceled points are not recommendations", search: `{"concurrency":[1,2,4],"min_session_completion":0.95}`,
			points: []BenchmarkPoint{
				{Seq: 1, Concurrency: 1, State: "done", Success: 52, OutputTput: 10, LongTail: complete},
				{Seq: 2, Concurrency: 2, State: "failed", Success: 52, OutputTput: 20, LongTail: complete},
				{Seq: 3, Concurrency: 4, State: "canceled", Success: 52, OutputTput: 30, LongTail: complete},
			}, best: 1,
		},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Open(fmt.Sprintf("file:benchmark_evidence_verdict_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", Mode: "grid", Search: tc.search})
			if err != nil {
				t.Fatal(err)
			}
			for _, point := range tc.points {
				// The stable proto wire embeds long_tail as an object, not a
				// string; this matches actual server/agent point reports.
				wire, err := json.Marshal(proto.BenchmarkPointResult{
					Seq: point.Seq, Concurrency: point.Concurrency, State: point.State,
					Success: point.Success, Errors: point.Errors, OutputTput: point.OutputTput,
					P99TTFT: point.P99TTFT, LongTail: json.RawMessage(point.LongTail),
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: wire}); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "done", StopReason: proto.BenchmarkStopGridExhausted}); err != nil {
				t.Fatal(err)
			}
			got, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var verdict BenchmarkVerdict
			if err := json.Unmarshal([]byte(got.Verdict), &verdict); err != nil {
				t.Fatalf("invalid stored verdict %q: %v", got.Verdict, err)
			}
			if verdict.Kind != "grid_done" || verdict.BestPointSeq != tc.best {
				t.Fatalf("persisted verdict=%s; want best_point_seq=%d", got.Verdict, tc.best)
			}
			if tc.best == 0 {
				var wire map[string]json.RawMessage
				if err := json.Unmarshal([]byte(got.Verdict), &wire); err != nil {
					t.Fatal(err)
				}
				if _, present := wire["best_point_seq"]; present {
					t.Fatalf("no eligible point must omit a recommendation: %s", got.Verdict)
				}
			}
		})
	}
}

func TestBenchmarkEvidenceCanceledPointReplacesRunningCurvePlaceholder(t *testing.T) {
	st, err := Open("file:benchmark_canceled_curve_evidence?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", Mode: "grid"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	for i, throughput := range []float64{2, 4} {
		if err := st.AppendBenchmarkCurveSample(run.ID, 4, start.Add(time.Duration(i)*time.Second), metrics.ProcessSample{OutputTput: throughput}); err != nil {
			t.Fatal(err)
		}
	}
	placeholder, err := st.GetBenchmarkPoint(run.ID, 4)
	if err != nil || placeholder.State != "running" || placeholder.Curve == "" || placeholder.ID == 0 || placeholder.CreatedAt.IsZero() {
		t.Fatalf("progress did not create a running curve placeholder: %+v err=%v", placeholder, err)
	}
	if err := st.CancelBenchmarkRun(run.ID, "operator canceled"); err != nil {
		t.Fatal(err)
	}
	canceledRun, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	const evidence = `{"Evidence":{"version":1,"plan":{"planned_sessions":1,"planned_requests":13},"outcomes":{"completed_sessions":0,"canceled_sessions":1,"canceled_before_start":11},"all":{"attempted":2,"success":1,"canceled":1}}}`
	partial := json.RawMessage(`{"id":999,"seq":4,"state":"canceled","concurrency":2,"success":1,"errors":1,"input_tokens":100,"output_tokens":20,"started_at":"2026-10-20T01:00:00Z","ended_at":"2026-10-20T01:00:02Z","long_tail":` + evidence + `}`)
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: partial}); err != nil {
		t.Fatal(err)
	}
	point, err := st.GetBenchmarkPoint(run.ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if point.State != "canceled" || point.Concurrency != 2 || point.Success != 1 || point.Errors != 1 || point.InputTokens != 100 || point.OutputTokens != 20 || point.LongTail != evidence {
		t.Fatalf("partial result did not replace placeholder facts: %+v", point)
	}
	if point.ID != placeholder.ID || point.RunID != placeholder.RunID || !point.CreatedAt.Equal(placeholder.CreatedAt) || point.Curve != placeholder.Curve {
		t.Fatalf("partial result replaced curve or row identity: before=%+v after=%+v", placeholder, point)
	}
	if point.StartedAt == nil || point.EndedAt == nil || !point.StartedAt.Equal(start) || !point.EndedAt.Equal(start.Add(2*time.Second)) {
		t.Fatalf("partial result lost the canceled measurement window: %+v", point)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "canceled", Point: json.RawMessage(`{"seq":4,"state":"canceled","errors":999,"long_tail":{"Evidence":{"version":999}}}`)}); err != nil {
		t.Fatal(err)
	}
	after, err := st.GetBenchmarkPoint(run.ID, 4)
	if err != nil || !reflect.DeepEqual(point, after) {
		t.Fatalf("duplicate delivery replaced finalized canceled evidence: before=%+v after=%+v err=%v", point, after, err)
	}
	afterRun, err := st.GetBenchmarkRun(run.ID)
	if err != nil || !reflect.DeepEqual(canceledRun, afterRun) {
		t.Fatalf("late partial result mutated canceled run: before=%+v after=%+v err=%v", canceledRun, afterRun, err)
	}
}

func TestBenchmarkEvidenceLateCanceledPointPreservesTerminalRun(t *testing.T) {
	tests := []struct {
		name, terminal, status, pointState string
		completedPoint, retained           bool
	}{
		{name: "partial point with running envelope", terminal: "canceled", status: "running", pointState: "canceled", retained: true},
		{name: "partial point with canceled envelope", terminal: "canceled", status: "canceled", pointState: "canceled", retained: true},
		{name: "late done envelope ignored", terminal: "canceled", status: "done", pointState: "canceled"},
		{name: "late failed envelope ignored", terminal: "canceled", status: "failed", pointState: "canceled"},
		{name: "late completed point ignored", terminal: "canceled", status: "running", pointState: "done"},
		{name: "late failed point ignored", terminal: "canceled", status: "running", pointState: "failed"},
		{name: "finished run remains finished", terminal: "done", status: "running", pointState: "canceled"},
		{name: "failed run remains failed", terminal: "failed", status: "canceled", pointState: "canceled"},
		{name: "previously completed point cannot be replaced", terminal: "canceled", status: "running", pointState: "canceled", completedPoint: true, retained: true},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, err := Open(fmt.Sprintf("file:benchmark_late_evidence_%d?mode=memory&cache=shared", i), bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", Mode: "grid"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.completedPoint {
				if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: json.RawMessage(`{"seq":2,"state":"done","success":13,"output_tput":10}`)}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.terminal == "canceled" {
				err = st.CancelBenchmarkRun(run.ID, "operator canceled")
			} else {
				err = st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: tc.terminal, Message: "original terminal message"})
			}
			if err != nil {
				t.Fatal(err)
			}
			before, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			late := json.RawMessage(fmt.Sprintf(`{"seq":2,"state":%q,"errors":1,"long_tail":{"Evidence":{"version":1,"all":{"attempted":1,"canceled":1},"outcomes":{"canceled_before_start":12}}}}`, tc.pointState))
			if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: tc.status, Message: "must not overwrite terminal message", Point: late}); err != nil {
				t.Fatal(err)
			}
			after, err := st.GetBenchmarkRun(run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("late point changed terminal run: before=%+v after=%+v err=%v", before, after, err)
			}
			points, err := st.ListBenchmarkPoints(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.retained {
				if len(points) != 0 {
					t.Fatalf("late non-cancellation data was retained: %+v", points)
				}
				return
			}
			if len(points) != 1 || points[0].Seq != 2 {
				t.Fatalf("late canceled point not retained exactly once: %+v", points)
			}
			if tc.completedPoint {
				if points[0].State != "done" || points[0].Success != 13 || points[0].OutputTput != 10 || points[0].Errors != 0 {
					t.Fatalf("completed point overwritten by late cancellation: %+v", points[0])
				}
				return
			}
			if points[0].State != "canceled" || points[0].Errors != 1 || points[0].LongTail != `{"Evidence":{"version":1,"all":{"attempted":1,"canceled":1},"outcomes":{"canceled_before_start":12}}}` {
				t.Fatalf("cancellation evidence damaged during persistence: %+v", points[0])
			}
			retained := points[0]
			if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: tc.status, Point: json.RawMessage(`{"seq":2,"state":"canceled","errors":999,"long_tail":{"Evidence":{"version":999}}}`)}); err != nil {
				t.Fatal(err)
			}
			points, err = st.ListBenchmarkPoints(run.ID)
			if err != nil || len(points) != 1 || !reflect.DeepEqual(points[0], retained) {
				t.Fatalf("duplicate delivery replaced retained evidence: points=%+v err=%v", points, err)
			}
		})
	}
}
