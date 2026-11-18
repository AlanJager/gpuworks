package app

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/logstore"
)

func TestRunPersistsAgentLogsAcceptedBeforeSessionDrain(t *testing.T) {
	a, _ := newTestApp(t, "gpuworks-test-password-2026")
	node, token, err := a.store.CreateNodePending("shutdown-log-node")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a.hub.SetSinks(a.obs.IngestMetrics, func(uuid string, lines []proto.LogLine) {
		close(entered)
		<-release // An accepted agent batch completes after Run was cancelled.
		a.obs.IngestLogs(uuid, lines)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	wireCtx, wireCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wireCancel()
	ws, _, err := websocket.Dial(wireCtx, "ws://"+a.Addr()+"/api/v1/agent/connect", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	ycfg := yamux.DefaultConfig()
	ycfg.LogOutput = io.Discard
	ysess, err := yamux.Client(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), ycfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ysess.Close()
	ctrl, err := ysess.OpenStream(wireCtx)
	if err != nil {
		t.Fatal(err)
	}
	encoder, decoder := json.NewEncoder(ctrl), json.NewDecoder(ctrl)
	if err := encoder.Encode(proto.StreamHeader{Kind: proto.StreamCtrl}); err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(proto.Hello{ProtoMin: 1, ProtoMax: 1, NodeToken: token})
	if err := encoder.Encode(proto.Envelope{T: proto.THello, D: hello}); err != nil {
		t.Fatal(err)
	}
	var registered proto.Envelope
	if err := decoder.Decode(&registered); err != nil || registered.T != proto.TRegistered {
		t.Fatalf("agent registration=%+v, %v", registered, err)
	}
	batch, _ := json.Marshal(proto.LogBatch{Lines: []proto.LogLine{{TimeMS: time.Now().UnixMilli(), Line: "accepted-before-stop", Source: logstore.SourceAgent}}})
	if err := encoder.Encode(proto.Envelope{T: proto.TLogs, D: batch}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("agent log batch was not accepted")
	}
	cancel()
	select {
	case <-ysess.CloseChan():
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not close the agent transport")
	}
	// Keep the accepted producer held beyond a consumer's ordinary flush
	// interval. Cancellation alone must not retire its log writer.
	time.Sleep(100 * time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after its accepted batch was released")
	}
	result, err := a.obs.Logs().Search(logstore.Filter{Node: node.UUID, Source: logstore.SourceAgent})
	if err != nil || len(result.Entries) != 1 || !strings.Contains(result.Entries[0].Line, "accepted-before-stop") {
		t.Fatalf("accepted log batch was not persisted: %+v, %v", result, err)
	}
}
