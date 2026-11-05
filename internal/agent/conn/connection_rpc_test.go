package conn

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestConnectionDiagnosticsRPCUsesReceivingConnectionWithoutProbes(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	handlers := &countingRPCHandlers{}
	c := New(Credentials{ServerURL: srv.URL, NodeToken: "local-secret"}, nil, handlers, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctrl := &nopRWC{}
	c.ctrl = ctrl
	c.handleRPC(context.Background(), proto.Envelope{T: proto.TRPCReq, ID: 42, D: json.RawMessage(`{"method":"node.connection_diagnostics","params":{}}`)})
	var env proto.Envelope
	if err := json.Unmarshal(ctrl.BytesCopy(), &env); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Result proto.DoctorReport `json:"result"`
		Error  string             `json:"error"`
	}
	if err := json.Unmarshal(env.D, &body); err != nil {
		t.Fatal(err)
	}
	if env.ID != 42 || env.T != proto.TRPCResp || body.Error != "" || proto.ConnectionValue(body.Result, "connection.source") != "agent_runtime" || proto.ConnectionValue(body.Result, "connection.server_url") != srv.URL {
		t.Fatalf("unexpected connection RPC result: %s", ctrl.BytesCopy())
	}
	if requests.Load() != 0 || len(handlers.methods) != 0 || strings.Contains(string(ctrl.BytesCopy()), "local-secret") {
		t.Fatal("snapshot must not call the host runtime, connect again or disclose its token")
	}
}

func TestConnectionDiagnosticsRejectsInputsBusyAndCancelled(t *testing.T) {
	c := New(Credentials{ServerURL: "https://node.example", NodeToken: "secret"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, raw := range []string{`{"url":"https://password@other.example"}`, `[]`, `{} {}`, `{`, strings.Repeat("x", 1025)} {
		if _, err := c.connectionDiagnostics(context.Background(), json.RawMessage(raw)); err == nil || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "other.example") {
			t.Fatalf("unsafe diagnostic input accepted or echoed: %v", err)
		}
	}
	c.diagnosticRunning.Store(true)
	if _, err := c.connectionDiagnostics(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("expected single-flight refusal, got %v", err)
	}
	c.diagnosticRunning.Store(false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.connectionDiagnostics(ctx, nil); err != context.Canceled {
		t.Fatalf("cancelled snapshot = %v", err)
	}
	if c.diagnosticRunning.Load() {
		t.Fatal("cancelled request retained its concurrency slot")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _, _ = c.connectionDiagnostics(context.Background(), json.RawMessage(`{}`)) })
	}
	wg.Wait()
	if _, err := c.connectionDiagnostics(context.Background(), nil); err != nil {
		t.Fatalf("concurrency slot not released: %v", err)
	}
}
