package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/debuglog"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The server opens and closes a node's debug session over RPC; the session
// file sits under the state directory and the agent's own log level is left
// alone. A node without the tee refuses rather than pretending.
func TestDebugSessionRPC(t *testing.T) {
	var main strings.Builder
	tee := debuglog.New(slog.NewTextHandler(&main, nil))
	log := slog.New(tee)
	n := newNodeRuntime(log, nil, t.TempDir(), "none", nil, nil)
	n.debugLog = tee
	ctx := context.Background()
	call := func(method string, params any) (proto.DebugSessionResult, error) {
		raw, _ := json.Marshal(params)
		out, err := n.OnRPC(ctx, method, raw)
		if err != nil {
			return proto.DebugSessionResult{}, err
		}
		return out.(proto.DebugSessionResult), nil
	}

	for _, bad := range []proto.DebugStartParams{
		{SessionID: "../x", UntilMS: time.Now().Add(time.Minute).UnixMilli(), MaxBytes: 1},
		{SessionID: "s", UntilMS: time.Now().Add(-time.Minute).UnixMilli(), MaxBytes: 1},
		{SessionID: "s", UntilMS: time.Now().Add(time.Minute).UnixMilli()},
	} {
		if _, err := call(proto.RPCDebugStart, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	got, err := call(proto.RPCDebugStart, proto.DebugStartParams{SessionID: "s1", UntilMS: time.Now().Add(time.Minute).UnixMilli(), MaxBytes: 10 << 30})
	if err != nil || !got.Open || got.Path != n.debugSessionDir()+"/s1.log" {
		t.Fatalf("start = %+v, %v", got, err)
	}
	if st, _ := tee.Status(); st.Limit != debugSessionMaxBytes {
		t.Fatalf("limit = %d, want the node cap", st.Limit)
	}
	log.Debug("runtime command", "args", "image inspect x")
	got, err = call(proto.RPCDebugStop, proto.DebugStopParams{SessionID: "s1"})
	if err != nil || got.Open || got.Ended != "stopped" {
		t.Fatalf("stop = %+v, %v", got, err)
	}
	body, _ := os.ReadFile(got.Path)
	if !strings.Contains(string(body), "runtime command") || strings.Contains(main.String(), "runtime command") {
		t.Fatalf("session file:\n%s\nagent log:\n%s", body, main.String())
	}
	if !strings.Contains(main.String(), "debug session started") {
		t.Fatalf("the start is recorded in the agent's own log:\n%s", main.String())
	}

	n.debugLog = nil
	if _, err := call(proto.RPCDebugStart, proto.DebugStartParams{SessionID: "s2", UntilMS: time.Now().Add(time.Minute).UnixMilli(), MaxBytes: 1}); err == nil {
		t.Fatal("a node without debug logging accepted a session")
	}
}

// A diagnostics read carries the agent's log and the named sessions' files,
// nothing the server names by path.
func TestStreamDiagnosticsCarriesTheAgentLogAndSessions(t *testing.T) {
	dir := t.TempDir()
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, t.TempDir(), "none", nil, nil)
	n.logDir = dir
	_ = os.WriteFile(dir+"/agent.log", []byte("current\n"), 0o600)
	_ = os.MkdirAll(dir+"/debug", 0o700)
	_ = os.WriteFile(dir+"/debug/s1.log", []byte("debug\n"), 0o600)
	_ = os.WriteFile(dir+"/debug/s2.log", []byte("other\n"), 0o600)
	var buf bytes.Buffer
	if err := n.StreamDiagnostics(context.Background(), []string{"s1", "../../etc/passwd", "missing"}, &buf); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if len(got) != 2 || got["agent.log"] != "current\n" || got["debug/s1.log"] != "debug\n" {
		t.Fatalf("tar = %v", got)
	}
}
