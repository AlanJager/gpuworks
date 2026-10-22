package store

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestBenchmarkCurveIsIncrementalBoundedAndPreservedByFinalPoint(t *testing.T) {
	st, err := Open("file:benchmark_curve?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "curve", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1_700_000_000, 0).UTC()
	for i := 0; i < 1000; i++ {
		if err := st.AppendBenchmarkCurveSample(run.ID, 1, start.Add(time.Duration(i)*time.Second), metrics.ProcessSample{OutputTput: float64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	point, err := st.GetBenchmarkPoint(run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var samples []BenchmarkCurveSample
	if err := json.Unmarshal([]byte(point.Curve), &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) > BenchmarkCurveMaxSamples || len(samples) == 0 {
		t.Fatalf("curve samples = %d, want 1..%d", len(samples), BenchmarkCurveMaxSamples)
	}
	if samples[0].T != start.UnixMilli() || samples[len(samples)-1].OutputTput != 999 {
		t.Fatalf("curve did not retain its span: first=%+v last=%+v", samples[0], samples[len(samples)-1])
	}
	if err := st.UpsertBenchmarkPoint(&BenchmarkPoint{RunID: run.ID, Seq: 1, State: "done", Concurrency: 8, OutputTput: 999}); err != nil {
		t.Fatal(err)
	}
	point, err = st.GetBenchmarkPoint(run.ID, 1)
	if err != nil || point.Curve == "" || point.State != "done" {
		t.Fatalf("final point = %+v, err=%v", point, err)
	}
}

// A hub status can be ignored successfully before a concurrent HTTP delete,
// while the same status's sample arrives at the curve entry afterward.
func TestBenchmarkDeletedRunCannotAcquireLateCurve(t *testing.T) {
	st, err := Open("file:benchmark_deleted_late_curve?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "canceled", Mode: "grid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", PointIndex: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteBenchmarkRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendBenchmarkCurveSample(run.ID, 1, time.Now(), metrics.ProcessSample{OutputTput: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBenchmarkRun(run.ID); err != ErrNotFound {
		t.Fatalf("parent did not stay deleted: %v", err)
	}
	points, err := st.ListBenchmarkPoints(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 0 {
		t.Fatalf("late curve recreated %d orphan point(s)", len(points))
	}
}

func TestBenchmarkTerminalRunRetainsFinalCurveWithoutChangingFacts(t *testing.T) {
	st, err := Open("file:benchmark_terminal_final_curve?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, state := range []string{"done", "failed", "canceled"} {
		t.Run(state, func(t *testing.T) {
			run, err := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: state, Mode: "grid", Error: "terminal reason"})
			if err != nil {
				t.Fatal(err)
			}
			point := BenchmarkPoint{RunID: run.ID, Seq: 1, State: state, Success: 7}
			if err := st.UpsertBenchmarkPoint(&point); err != nil {
				t.Fatal(err)
			}
			if err := st.AppendBenchmarkCurveSample(run.ID, 1, time.Now(), metrics.ProcessSample{OutputTput: 4}); err != nil {
				t.Fatal(err)
			}
			after, err := st.GetBenchmarkRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(run, after) {
				t.Fatalf("final curve changed terminal parent: before=%+v after=%+v", run, after)
			}
			saved, err := st.GetBenchmarkPoint(run.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			var samples []BenchmarkCurveSample
			if err := json.Unmarshal([]byte(saved.Curve), &samples); err != nil {
				t.Fatal(err)
			}
			if saved.ID != point.ID || saved.State != point.State || saved.Success != point.Success || len(samples) != 1 || samples[0].OutputTput != 4 {
				t.Fatalf("final curve changed result identity/facts: %+v", saved)
			}
		})
	}
}
