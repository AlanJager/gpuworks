package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestSourceProbeAnswersEachAddressInOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models/Qwen/Qwen3-0.6B/revision/main" {
			_, _ = w.Write([]byte(`{"sha":"abc"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + listener.Addr().String()
	_ = listener.Close()
	n := &NodeRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	raw, _ := json.Marshal(proto.SourceProbeParams{Source: "huggingface", Repo: "Qwen/Qwen3-0.6B", Bases: []string{dead, server.URL}})
	got, err := n.OnRPC(context.Background(), proto.RPCSourceProbe, raw)
	if err != nil {
		t.Fatal(err)
	}
	result := got.(proto.SourceProbeResult)
	if len(result.Outcomes) != 2 || result.Outcomes[0].Base != dead || result.Outcomes[0].Status != proto.SourceProbeUnreachable ||
		result.Outcomes[1].Base != server.URL || result.Outcomes[1].Status != proto.SourceProbeOK {
		t.Fatalf("outcomes = %+v", result.Outcomes)
	}

	for _, bad := range []proto.SourceProbeParams{
		{Source: "url", Repo: "x", Bases: []string{server.URL}},
		{Source: "huggingface", Bases: []string{server.URL}},
		{Source: "huggingface", Repo: "x"},
	} {
		raw, _ := json.Marshal(bad)
		if _, err := n.OnRPC(context.Background(), proto.RPCSourceProbe, raw); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
}
