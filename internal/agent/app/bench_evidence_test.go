package app

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func agentEvidenceArtifact(t *testing.T, results []loadgen.Result) ([]byte, int) {
	t.Helper()
	var statuses []proto.BenchmarkStatus
	engine := newBenchEngine(func(st proto.BenchmarkStatus) { statuses = append(statuses, st) }, slog.Default())
	engine.reportDetail(71, 3, 1, results)
	if len(statuses) == 0 {
		t.Fatal("no artifact chunks reported")
	}
	var compressed []byte
	for i, status := range statuses {
		if status.ID != 71 || status.PointIndex != 3 || status.State != "running" || status.DetailSeq != i || status.DetailFinal != (i == len(statuses)-1) {
			t.Fatalf("chunk %d has invalid association or finality: %+v", i, status)
		}
		if len(status.DetailChunk) > benchmarkDetailChunkSize {
			t.Fatalf("chunk %d exceeds transport bound: %d", i, len(status.DetailChunk))
		}
		compressed = append(compressed, status.DetailChunk...)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("artifact chunks do not form complete gzip: %v", err)
	}
	return raw, len(statuses)
}

func TestAgentDetailEvidenceWirePreservesIdentityTimingAndErrors(t *testing.T) {
	start := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	localInput, localOutput := 9, 3
	failure := errors.New("stream stopped: \"peer closed\"\npartial output retained")
	raw, _ := agentEvidenceArtifact(t, []loadgen.Result{
		{
			Metrics: metrics.Request{
				SessionID: "session-2", Turn: 3, PlannedTurns: 13,
				StartedAt: start, FirstOutputAt: start.Add(100 * time.Millisecond), FirstContentAt: start.Add(300 * time.Millisecond),
				LastOutputAt: start.Add(800 * time.Millisecond), FinishedAt: start.Add(time.Second),
				TTFT: 100 * time.Millisecond, Latency: 800 * time.Millisecond, TPOT: 700 * time.Millisecond,
				InputTokens: 10, OutputTokens: 3, Chunks: 2, ITL: []time.Duration{700 * time.Millisecond},
				LocalInputTokens: &localInput, LocalOutputTokens: &localOutput,
				InputSource: "server_usage", OutputSource: "server_usage", FinishReason: "stop", HTTPStatus: 200,
			},
			UsageJSON: json.RawMessage(`{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":0}}`),
			Text:      "answer", OutputBudget: 220, Stream: true,
		},
		{
			Metrics: metrics.Request{
				SessionID: "session-4", Turn: 1, PlannedTurns: 13, Error: failure, ErrorStage: "http", HTTPStatus: 503,
				StartedAt: start.Add(2 * time.Second), FinishedAt: start.Add(4 * time.Second),
			},
			UsageJSON: json.RawMessage(`null`), OutputBudget: 220, Stream: true,
		},
	})
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("want two independently valid JSONL records, got %q", raw)
	}
	var successful map[string]any
	if err := json.Unmarshal(lines[0], &successful); err != nil {
		t.Fatal(err)
	}
	wantIdentity := map[string]any{
		"version": float64(1), "run_id": float64(71), "point_seq": float64(3), "session_id": "session-2",
		"turn": float64(3), "planned_turns": float64(13), "attempt": float64(1), "finish_reason": "stop",
		"http_status": float64(200), "output_budget": float64(220), "stream": true, "text": "answer",
		"local_input": float64(9), "local_output": float64(3), "input_source": "server_usage", "output_source": "server_usage",
	}
	for key, want := range wantIdentity {
		if !reflect.DeepEqual(successful[key], want) {
			t.Errorf("%s = %#v, want %#v", key, successful[key], want)
		}
	}
	var wantMetrics map[string]any
	if err := json.Unmarshal([]byte(`{
		"started_at":"2026-10-20T01:02:03Z","first_output_at":"2026-10-20T01:02:03.1Z",
		"first_content_at":"2026-10-20T01:02:03.3Z","last_output_at":"2026-10-20T01:02:03.8Z",
		"finished_at":"2026-10-20T01:02:04Z","ttft_ms":100,"tpot_ms":350,"latency_ms":800,
		"completion_ms":1000,"input_tokens":10,"output_tokens":3,"chunks":2,"chunk_intervals_ms":[700]
	}`), &wantMetrics); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(successful["metrics"], wantMetrics) {
		t.Fatalf("timing facts or units changed: got %#v, want %#v", successful["metrics"], wantMetrics)
	}
	usage, ok := successful["usage"].(map[string]any)
	if !ok || usage["prompt_tokens_details"].(map[string]any)["cached_tokens"] != float64(0) {
		t.Fatalf("explicit zero cache usage lost: %#v", successful["usage"])
	}
	var failed map[string]any
	if err := json.Unmarshal(lines[1], &failed); err != nil {
		t.Fatalf("error text broke JSONL escaping: %v", err)
	}
	if failed["error_text"] != failure.Error() || failed["error_stage"] != "http" || failed["http_status"] != float64(503) || failed["session_id"] != "session-4" || failed["turn"] != float64(1) {
		t.Fatalf("failed request cannot be traced: %#v", failed)
	}
	if usage, present := failed["usage"]; !present || usage != nil {
		t.Fatalf("explicit null usage became absent or fabricated: %#v", failed)
	}
	failedMetrics := failed["metrics"].(map[string]any)
	if failedMetrics["started_at"] != "2026-10-20T01:02:05Z" || failedMetrics["finished_at"] != "2026-10-20T01:02:07Z" || failedMetrics["completion_ms"] != float64(2000) {
		t.Fatalf("failure timing lost: %#v", failedMetrics)
	}
	for _, key := range []string{"first_output_at", "first_content_at", "last_output_at", "ttft_ms", "tpot_ms", "latency_ms"} {
		if value, present := failedMetrics[key]; !present || value != nil {
			t.Errorf("unobserved %s must be explicit null: %#v", key, failedMetrics)
		}
	}
	if _, present := failedMetrics["Error"]; present {
		t.Fatal("Go error interface leaked into machine metrics")
	}
}

func TestAgentDetailEvidenceChunkingPreservesRecordsBeforeTruncation(t *testing.T) {
	noise := make([]byte, 180*1024)
	if _, err := rand.New(rand.NewSource(73)).Read(noise); err != nil {
		t.Fatal(err)
	}
	text := base64.StdEncoding.EncodeToString(noise)
	raw, chunks := agentEvidenceArtifact(t, []loadgen.Result{
		{Metrics: metrics.Request{SessionID: "retained", Turn: 1, PlannedTurns: 1}, Text: text},
		{Metrics: metrics.Request{SessionID: "too-large", Turn: 1, PlannedTurns: 1}, Text: strings.Repeat("x", 2*1024*1024)},
		{Metrics: metrics.Request{SessionID: "after-cap", Turn: 1, PlannedTurns: 1}, Text: "must not appear"},
	})
	if chunks < 2 {
		t.Fatalf("fixture did not exercise real transport chunking: chunks=%d", chunks)
	}
	if len(raw) > 1024*1024 {
		t.Fatalf("raw artifact cap exceeded: %d", len(raw))
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	if len(lines) != 2 || string(lines[1]) != `{"truncated":true}` {
		t.Fatalf("want one complete record and a terminal truncation marker, lines=%d", len(lines))
	}
	var retained map[string]any
	if err := json.Unmarshal(lines[0], &retained); err != nil {
		t.Fatal(err)
	}
	if retained["session_id"] != "retained" || retained["text"] != text || retained["version"] != float64(1) || retained["run_id"] != float64(71) || retained["point_seq"] != float64(3) {
		t.Fatal("record identity or payload was damaged across gzip chunks")
	}
}
