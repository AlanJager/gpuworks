package conn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestConnectivityDiagnoseUsesSmokeWire(t *testing.T) {
	peer := &laneTestServer{sessionID: "not-a-diagnostic-session"}
	server := httptest.NewServer(http.HandlerFunc(peer.handle))
	defer server.Close()
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: server.URL, NodeToken: "node-private-token"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"source": "cli", "outcome": "ok", "control": "smoke_verified", "data_lane": "unverified", "phase.wait_registered": "ok", "phase.ws_upgrade": "ok", "proxy": "direct"} {
		if got := proto.ConnectionValue(report, "connection."+key); got != want {
			t.Fatalf("%s=%q want %q: %+v", key, got, want, report.Facts)
		}
	}
	lanes, data := peer.snapshot()
	if len(lanes) != 1 || lanes[0] != "" || len(data) != 0 {
		t.Fatalf("diagnosis changed lane contract: %v %v", lanes, data)
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), "node-private-token") || strings.Contains(string(b), "not-a-diagnostic-session") {
		t.Fatal("credential leaked")
	}
}

func TestConnectivityInvalidConfigNeverLeaks(t *testing.T) {
	var log bytes.Buffer
	report, err := DiagnoseConnectivity(context.Background(), Credentials{ServerURL: "http://user:proxy-private@example.invalid/p/node-private?token=query-private", NodeToken: "node-private"}, slog.New(slog.NewTextHandler(&log, nil)))
	if err == nil || proto.ConnectionValue(report, "connection.outcome") != "invalid_configuration" {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	b, _ := json.Marshal(report)
	for _, secret := range []string{"proxy-private", "node-private", "query-private"} {
		if strings.Contains(string(b)+log.String()+err.Error(), secret) {
			t.Fatalf("leaked %q", secret)
		}
	}
}

func TestConnectivityTimeoutClosesOwnRequest(t *testing.T) {
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done(); close(released) }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	report, err := DiagnoseConnectivity(ctx, Credentials{ServerURL: server.URL, NodeToken: "private-token"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !errors.Is(err, context.DeadlineExceeded) || proto.ConnectionValue(report, "connection.reason") != "timeout" {
		t.Fatalf("report=%+v err=%v", report.Facts, err)
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("diagnostic left HTTP request alive")
	}
}

func TestConnectivitySnapshotIsPureAndRetryIsBounded(t *testing.T) {
	var log bytes.Buffer
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer server.Close()
	c := New(Credentials{ServerURL: server.URL + "/prefix", NodeToken: "private-token"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(&log, nil)))
	before := c.ConnectionReport()
	if proto.ConnectionValue(before, "connection.source") != "agent_runtime" || proto.ConnectionValue(before, "connection.control") != "unknown" || requests.Load() != 0 {
		t.Fatalf("new snapshot did I/O: %+v", before.Facts)
	}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("expected WS upgrade failure")
	}
	c.RecordReconnect(time.Minute)
	after := c.ConnectionReport()
	at, err := time.Parse(time.RFC3339Nano, proto.ConnectionValue(after, "connection.retry_at"))
	if err != nil || time.Until(at) < 59*time.Second {
		t.Fatalf("retry_at=%v %v", at, err)
	}
	c.RecordReconnect(time.Minute)
	if strings.Count(log.String(), "connection_event_v1") != 1 || strings.Contains(log.String(), "private-token") {
		t.Fatal("missing/duplicate/unsafe failure event")
	}
}

func TestConnectivityRuntimeSnapshotConcurrentWithDataAndCancel(t *testing.T) {
	peer := &laneTestServer{sessionID: "runtime-session-private"}
	c, stop := runAgainst(t, peer, true)
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			if err := stop(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(shutdown)
	deadline := time.Now().Add(3 * time.Second)
	for proto.ConnectionValue(c.ConnectionReport(), "connection.data_lane") != "attached" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if report := c.ConnectionReport(); proto.ConnectionValue(report, "connection.control") != "connected" || proto.ConnectionValue(report, "connection.data_lane") != "attached" {
		t.Fatalf("actual runtime state absent: %+v", report.Facts)
	}
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			for range 100 {
				report := c.ConnectionReport()
				if proto.ConnectionValue(report, "connection.source") != "agent_runtime" {
					t.Error("wrong process source")
				}
				data, _ := json.Marshal(report)
				if strings.Contains(string(data), "runtime-session-private") {
					t.Error("session leaked")
				}
			}
		})
	}
	shutdown()
	wg.Wait()
	if proto.ConnectionValue(c.ConnectionReport(), "connection.control") != "not_connected" {
		t.Fatal("closed runtime still connected")
	}
}
