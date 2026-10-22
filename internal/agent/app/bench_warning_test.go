package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestAgentBenchmarkReportsTokenizerFallbackWarning(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(fixture.Close)
	var statuses []proto.BenchmarkStatus
	engine := newBenchEngine(func(status proto.BenchmarkStatus) {
		statuses = append(statuses, status)
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.run(context.Background(), proto.BenchmarkSpec{
		ID: 17, TargetURL: fixture.URL, Model: "m", Mode: "grid",
		Search:       json.RawMessage(`{"concurrency":[1],"isl":[2],"osl":[1],"requests_per_point_multiplier":1}`),
		Dataset:      json.RawMessage(`{"kind":"random","input_tokens":2,"output_tokens":1}`),
		TokenizerRef: filepath.Join(t.TempDir(), "missing-tokenizer.json"),
	})
	if len(statuses) < 3 || statuses[len(statuses)-1].State != "done" {
		t.Fatalf("statuses = %+v", statuses)
	}
	for _, status := range statuses {
		if len(status.Warnings) != 1 || status.Warnings[0] != proto.BenchmarkWarningTokenizerLoad {
			t.Fatalf("status did not retain tokenizer fallback warning: %+v", status)
		}
	}
}
