package conn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSmokeDoesNotProbeAccelerators(t *testing.T) {
	called := false
	orig := discoverProber
	discoverProber = func() gpu.Prober {
		called = true
		return gpu.NoAccelerator{}
	}
	t.Cleanup(func() { discoverProber = orig })

	c := New(Credentials{Smoke: true, NodeToken: "t"}, nil, nil, nil, slog.Default())
	if called {
		t.Fatal("smoke probed accelerators")
	}
	if _, ok := c.prober.(gpu.NoAccelerator); !ok {
		t.Fatalf("smoke prober = %T, want NoAccelerator", c.prober)
	}
	_ = New(Credentials{NodeToken: "t"}, nil, nil, nil, slog.Default())
	if !called {
		t.Fatal("a normal connection did not probe accelerators")
	}
}

func TestSmokeDeadlineNamesDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	c := New(Credentials{Smoke: true, ServerURL: "http://127.0.0.1:1", NodeToken: "node-token"}, nil, nil, nil, log)
	err := c.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "dial=") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "node-token") {
		t.Fatalf("token leaked into smoke error: %v", err)
	}
	if !strings.Contains(buf.String(), "smoke stages") {
		t.Fatalf("log = %s", buf.String())
	}
}

func TestSmokeHandshakeRecordsStages(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	c := New(Credentials{Smoke: true, NodeToken: "old-token"}, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.trace = newSmokeTrace()
	c.ctrl = client
	c.dec = json.NewDecoder(bufio.NewReader(client))
	done := make(chan struct{})
	go func() {
		defer close(done)
		var hello proto.Envelope
		if err := json.NewDecoder(server).Decode(&hello); err != nil {
			return
		}
		reg, _ := json.Marshal(proto.Envelope{T: proto.TRegistered, D: json.RawMessage(`{"node_uuid":"node-new","node_token":"new-token"}`)})
		welcome, _ := json.Marshal(proto.Envelope{T: proto.THello, D: json.RawMessage(`{"proto_min":1,"proto_max":1}`)})
		_, _ = server.Write(append(reg, '\n'))
		_, _ = server.Write(append(welcome, '\n'))
	}()
	if err := c.handshake(); err != nil {
		t.Fatal(err)
	}
	got := c.trace.String()
	for _, stage := range []string{"hello=", "wait_registered=", "server_hello="} {
		if !strings.Contains(got, stage) {
			t.Fatalf("stages %q missing %s", got, stage)
		}
	}
	if strings.Contains(got, "old-token") || strings.Contains(got, "new-token") {
		t.Fatalf("token leaked into stages: %s", got)
	}
	<-done
}

func TestFinishSmokeKeepsReject(t *testing.T) {
	c := &Conn{trace: newSmokeTrace(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	end := c.smokeStage("wait_registered")
	end()
	err := c.finishSmoke(&RejectError{Reason: "bad_token"})
	var rej *RejectError
	if !errors.As(err, &rej) || rej.Reason != "bad_token" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "wait_registered=") {
		t.Fatalf("stage missing: %v", err)
	}
}

func TestSmokeStageStaysOpenUntilFinished(t *testing.T) {
	c := &Conn{trace: newSmokeTrace()}
	end := c.smokeStage("wait_registered")
	if got := c.trace.String(); !strings.Contains(got, "wait_registered=") {
		t.Fatalf("open stage = %q", got)
	}
	end()
	end()
	if strings.Count(c.trace.String(), "wait_registered=") != 1 {
		t.Fatalf("stage recorded twice: %s", c.trace.String())
	}
}
