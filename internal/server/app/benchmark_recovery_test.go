package app

import (
	"log/slog"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestNewRecoversOnlyServerOriginBenchmarkRuns(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		Addr:          "127.0.0.1:0",
		DataDir:       dir,
		DBPath:        filepath.Join(dir, "gpuworks.db"),
		AdminUser:     "admin",
		AdminPassword: "benchmark-recovery-password",
	}
	first, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	// New starts the proxy's route-table goroutine outside of Run; this test
	// never calls Run, so close it explicitly (ordered before the store close
	// three lines down) rather than leak it ticking against a closed db for
	// the rest of the test binary.
	defer first.proxy.Close()
	serverRun, err := first.Store().CreateBenchmarkRun(&store.BenchmarkRun{LoadOrigin: "server", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	agentRun, err := first.Store().CreateBenchmarkRun(&store.BenchmarkRun{LoadOrigin: "node-1", State: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Store().UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: serverRun.ID, Seq: 1, OutputTput: 42}); err != nil {
		t.Fatal(err)
	}
	if err := first.Store().Close(); err != nil {
		t.Fatal(err)
	}

	second, err := New(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { second.proxy.Close(); _ = second.Store().Close() })
	serverRun, _ = second.Store().GetBenchmarkRun(serverRun.ID)
	agentRun, _ = second.Store().GetBenchmarkRun(agentRun.ID)
	points, _ := second.Store().ListBenchmarkPoints(serverRun.ID)
	if serverRun.State != "failed" || serverRun.Error != store.BenchmarkErrorServerRestarted || serverRun.Verdict == "" || len(points) != 1 {
		t.Fatalf("server run=%+v points=%+v", serverRun, points)
	}
	if agentRun.State != "running" {
		t.Fatalf("agent-origin run should remain reconcilable: %+v", agentRun)
	}
}
