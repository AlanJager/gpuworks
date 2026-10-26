package app

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func TestBenchDetailIsChunkedGzipJSONL(t *testing.T) {
	var got []proto.BenchmarkStatus
	e := newBenchEngine(func(st proto.BenchmarkStatus) { got = append(got, st) }, slog.Default())
	e.reportDetail(7, 2, 1, []loadgen.Result{{Metrics: metrics.Request{InputTokens: 3, OutputTokens: 2}, Text: "ok"}})
	if len(got) != 1 || !got[0].DetailFinal || len(got[0].DetailChunk) == 0 {
		t.Fatalf("statuses=%+v", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(got[0].DetailChunk))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(b), &line); err != nil {
		t.Fatalf("artifact=%q: %v", b, err)
	}
	if line["text"] != "ok" {
		t.Fatalf("line=%v", line)
	}
}

func TestBenchDetailCapProducesTruncatedMarker(t *testing.T) {
	var got []proto.BenchmarkStatus
	e := newBenchEngine(func(st proto.BenchmarkStatus) { got = append(got, st) }, slog.Default())
	e.reportDetail(8, 1, 1, []loadgen.Result{{Text: strings.Repeat("x", 2*1024*1024)}})
	var compressed []byte
	for _, st := range got {
		compressed = append(compressed, st.DetailChunk...)
	}
	if len(got) == 0 || !got[len(got)-1].DetailFinal {
		t.Fatalf("statuses=%+v", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil || strings.TrimSpace(string(b)) != `{"truncated":true}` {
		t.Fatalf("artifact=%q err=%v", b, err)
	}
}

func TestBenchmarkStatusesBufferAcrossDisconnect(t *testing.T) {
	n := &NodeRuntime{}
	n.reportBenchmark(proto.BenchmarkStatus{ID: 9, State: "running"})
	var got []proto.BenchmarkStatus
	n.Bind(func(kind string, value any) error {
		if kind != proto.TStatusBenchmark {
			t.Fatalf("kind=%s", kind)
		}
		got = append(got, value.(proto.BenchmarkStatus))
		return nil
	})
	if len(got) != 1 || got[0].ID != 9 {
		t.Fatalf("replayed statuses=%+v", got)
	}
	n.Unbind()
	n.reportBenchmark(proto.BenchmarkStatus{ID: 9, State: "done"})
	n.Bind(func(kind string, value any) error { got = append(got, value.(proto.BenchmarkStatus)); return nil })
	if len(got) != 2 || got[1].State != "done" {
		t.Fatalf("replayed terminal status=%+v", got)
	}
}

func TestBenchmarkCancelRPCWaitsForAgentExecutionToStop(t *testing.T) {
	started := make(chan struct{})
	requestCanceled := make(chan struct{})
	var requests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done()
		select {
		case <-requestCanceled:
		default:
			close(requestCanceled)
		}
	}))
	defer fixture.Close()

	var statusesMu sync.Mutex
	var statuses []proto.BenchmarkStatus
	engine := newBenchEngine(func(st proto.BenchmarkStatus) {
		statusesMu.Lock()
		statuses = append(statuses, st)
		statusesMu.Unlock()
	}, slog.Default())
	node := &NodeRuntime{bench: engine}
	engine.ApplyUpdate(context.Background(), []proto.BenchmarkSpec{{
		TokenizerRef: agentBenchmarkTestTokenizer(t), ID: 41, TargetURL: fixture.URL, Model: "m", Mode: "grid",
		Dataset: json.RawMessage(`{"kind":"random","input_tokens":8,"output_tokens":4}`),
		Search:  json.RawMessage(`{"concurrency":1,"requests_per_point_multiplier":3}`),
	}}, nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("agent benchmark request did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	resultAny, err := node.OnRPC(ctx, proto.RPCBenchCancel, json.RawMessage(`{"id":41}`))
	if err != nil {
		t.Fatal(err)
	}
	result, ok := resultAny.(proto.BenchmarkCancelResult)
	if !ok || result.ID != 41 || result.State != "canceled" || result.Running {
		t.Fatalf("result=%#v", resultAny)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not observe cancellation after the RPC acknowledgement")
	}
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests=%d, agent sent queued requests after cancellation", got)
	}
	statusesMu.Lock()
	defer statusesMu.Unlock()
	if len(statuses) == 0 || statuses[len(statuses)-1].State != "canceled" {
		t.Fatalf("statuses=%+v", statuses)
	}
}

func TestBenchmarkDesiredRemovalCancelsAgentExecution(t *testing.T) {
	started := make(chan struct{})
	requestCanceled := make(chan struct{})
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(requestCanceled)
	}))
	defer fixture.Close()
	e := newBenchEngine(func(proto.BenchmarkStatus) {}, slog.Default())
	e.ApplyUpdate(context.Background(), []proto.BenchmarkSpec{{
		TokenizerRef: agentBenchmarkTestTokenizer(t), ID: 42, TargetURL: fixture.URL, Model: "m", Mode: "grid",
		Dataset: json.RawMessage(`{"kind":"random","input_tokens":8,"output_tokens":4}`),
		Search:  json.RawMessage(`{"concurrency":1}`),
	}}, nil)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("agent benchmark request did not start")
	}
	e.SetDesiredFull(context.Background(), nil)
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("full desired-state removal did not cancel the in-flight request")
	}
}

func agentBenchmarkTestTokenizer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"a":0,"b":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
