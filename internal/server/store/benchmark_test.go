package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestBenchmarkRunAndStatusPersistence(t *testing.T) {
	st, err := Open("file:benchmark_store_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "smoke", LoadOrigin: "server", BaseURL: "http://fixture", Dataset: `{"kind":"random"}`})
	if err != nil {
		t.Fatal(err)
	}
	if run.NumGPUs != 0 {
		t.Fatalf("default num_gpus=%d, want 0 (unknown)", run.NumGPUs)
	}
	result, _ := json.Marshal(map[string]any{"output_tput": 12.5})
	point, _ := json.Marshal(BenchmarkPoint{Seq: 1, Concurrency: 2, State: "done", OutputTput: 12.5})
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "done", Result: result, Point: point}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" {
		t.Fatalf("state=%q", got.State)
	}
	points, err := st.ListBenchmarkPoints(run.ID)
	if err != nil || len(points) != 1 || points[0].OutputTput != 12.5 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
}

func TestBenchmarkPointWirePersistsPeaksAndTiming(t *testing.T) {
	st, err := Open("file:benchmark_point_wire?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server"})
	started := time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC)
	ended := started.Add(12 * time.Second)
	point, _ := json.Marshal(map[string]any{
		"seq": 1, "state": "done", "max_output_tput": 42.5, "max_total_tput": 84.5, "request_rate": 2.5, "concurrency": 16,
		"started_at": started, "ended_at": ended,
	})
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", Point: point}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBenchmarkPoint(run.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxOutputTput != 42.5 || got.MaxTotalTput != 84.5 || got.RequestRate != 2.5 || got.Concurrency != 16 || got.StartedAt == nil || !got.StartedAt.Equal(started) || got.EndedAt == nil || !got.EndedAt.Equal(ended) {
		t.Fatalf("persisted point = %+v", got)
	}
}

func TestBenchmarkStatusRejectsWrongAgent(t *testing.T) {
	st, err := Open("file:benchmark_status_node_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, _, err := st.CreateNodePending("node", "agent_dial")
	if err != nil {
		t.Fatal(err)
	}
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "smoke", LoadOrigin: "other-node"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(n.ID, proto.BenchmarkStatus{ID: run.ID, State: "running"}); err == nil {
		t.Fatal("expected node mismatch")
	}
}

func TestBenchmarkStatusDoesNotRegress(t *testing.T) {
	st, err := Open("file:benchmark_status_transition_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, err := st.CreateBenchmarkRun(&BenchmarkRun{Name: "transition", LoadOrigin: "server"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "pending"}); err == nil {
		t.Fatal("expected running -> pending rejection")
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "failed"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" {
		t.Fatalf("terminal state regressed: %q", got.State)
	}
}

func TestBenchmarkProgressPersistsCurrentPointAndTrueRequestTotal(t *testing.T) {
	st, err := Open("file:benchmark_progress_point?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	run, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server"})
	current := &proto.BenchmarkCurrentPoint{Seq: 3, Concurrency: 4, TargetISL: 4096, TargetOSL: 1024, RequestTotal: 40}
	if err := st.ApplyBenchmarkStatus(0, proto.BenchmarkStatus{ID: run.ID, State: "running", PointIndex: 3, Completed: 32, CurrentPoint: current}); err != nil {
		t.Fatal(err)
	}
	run, _ = st.GetBenchmarkRun(run.ID)
	var progress proto.BenchmarkStatus
	if err := json.Unmarshal([]byte(run.Progress), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.CurrentPoint == nil || *progress.CurrentPoint != *current || progress.Completed != 32 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestRecoverServerBenchmarkRunsPreservesAgentAndPoints(t *testing.T) {
	st, err := Open("file:benchmark_orphan_recovery?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	serverRun, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "running"})
	agentRun, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "node-1", State: "running"})
	if err := st.UpsertBenchmarkPoint(&BenchmarkPoint{RunID: serverRun.ID, Seq: 1, OutputTput: 42}); err != nil {
		t.Fatal(err)
	}
	count, err := st.RecoverServerBenchmarkRuns()
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	serverRun, _ = st.GetBenchmarkRun(serverRun.ID)
	agentRun, _ = st.GetBenchmarkRun(agentRun.ID)
	points, _ := st.ListBenchmarkPoints(serverRun.ID)
	if serverRun.State != "failed" || serverRun.Error != BenchmarkErrorServerRestarted || serverRun.EndedAt == nil || len(points) != 1 {
		t.Fatalf("server=%+v points=%+v", serverRun, points)
	}
	if agentRun.State != "running" {
		t.Fatalf("agent run should be untouched: %+v", agentRun)
	}
}

func TestDeleteBenchmarkRunCascadesPointsAndRejectsActive(t *testing.T) {
	st, err := Open("file:benchmark_delete?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	active, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "running"})
	if err := st.DeleteBenchmarkRun(active.ID); !errors.Is(err, ErrBenchmarkActive) {
		t.Fatalf("active delete err=%v", err)
	}
	done, _ := st.CreateBenchmarkRun(&BenchmarkRun{LoadOrigin: "server", State: "done"})
	_ = st.UpsertBenchmarkPoint(&BenchmarkPoint{RunID: done.ID, Seq: 1})
	if err := st.DeleteBenchmarkRun(done.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBenchmarkRun(done.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("run lookup err=%v", err)
	}
	points, _ := st.ListBenchmarkPoints(done.ID)
	if len(points) != 0 {
		t.Fatalf("orphan points=%+v", points)
	}
}
