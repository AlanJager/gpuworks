package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/libp2p/go-yamux/v5"

	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// A refused node token is the one rejection an operator on the host has to
// act on: the log names the remedies. The other reasons resolve by themselves
// (a new control session, an upgrade) and carry no step.
func TestRejectionNextStepNamesTheRemedyForAnUnknownNodeToken(t *testing.T) {
	next := rejectionNextStep(proto.RejectBadToken)
	for _, want := range []string{"node token", "gpuworks-agent uninstall", "install command", "another agent"} {
		if !strings.Contains(next, want) {
			t.Fatalf("next step %q does not mention %q", next, want)
		}
	}
	for _, reason := range []string{proto.RejectStaleSession, proto.RejectProtoMismatch, "transport", ""} {
		if got := rejectionNextStep(reason); got != "" {
			t.Fatalf("reason %q carries a next step: %q", reason, got)
		}
	}
}

// The remedy has to be on the line the agent logs when a real server refuses
// its node token, and nowhere else: a server that refuses the handshake with
// bad_token, through the connection the agent runs, ends in one error line
// that names the reason and the next step. The token and the server's own
// detail are not on it.
func TestARefusedNodeTokenLogsItsRemedy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		session, err := yamux.Server(websocket.NetConn(context.Background(), ws, websocket.MessageBinary), yamux.DefaultConfig(), nil)
		if err != nil {
			return
		}
		defer session.Close()
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		defer stream.Close()
		reader := bufio.NewReader(stream)
		_, _ = reader.ReadString('\n') // the stream's kind
		var hello proto.Envelope
		_ = json.NewDecoder(reader).Decode(&hello)
		body, _ := json.Marshal(proto.Reject{Reason: proto.RejectBadToken, Detail: "detail-from-the-server"})
		_ = json.NewEncoder(stream).Encode(proto.Envelope{T: proto.TReject, D: body})
		_, _ = io.Copy(io.Discard, reader)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := conn.New(conn.Credentials{ServerURL: server.URL, NodeUUID: "refused-node", NodeToken: "the-node-token"}, nil, nil, gpu.NoAccelerator{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var rejected *conn.RejectError
	if err := c.Run(ctx); !errors.As(err, &rejected) || rejected.Reason != proto.RejectBadToken {
		t.Fatalf("run = %v, want the server's refusal of the node token", err)
	}

	var out bytes.Buffer
	logConnectionFailure(slog.New(slog.NewTextHandler(&out, nil)), c, true)
	line := out.String()
	if strings.Count(line, "\n") != 1 || !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "reason=bad_token") {
		t.Fatalf("log = %q, want one error line naming the reason", line)
	}
	if !strings.Contains(line, "next=") || !strings.Contains(line, rejectionNextStep(proto.RejectBadToken)) {
		t.Fatalf("log = %q, want the next step on the refusal's own line", line)
	}
	for _, private := range []string{"the-node-token", "detail-from-the-server"} {
		if strings.Contains(line, private) {
			t.Fatalf("log carries %q: %s", private, line)
		}
	}

	// A connection that merely ended is not a refusal and names no step.
	out.Reset()
	logConnectionFailure(slog.New(slog.NewTextHandler(&out, nil)), c, false)
	if strings.Contains(out.String(), "next=") || !strings.Contains(out.String(), "level=WARN") {
		t.Fatalf("ended connection log = %q", out.String())
	}
}
