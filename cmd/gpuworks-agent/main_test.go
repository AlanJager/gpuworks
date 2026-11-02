package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunRejectsServerURLWithoutScheme(t *testing.T) {
	t.Setenv("GPUWORKS_STATE_DIR", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(log, []string{"--server", "sa1.tunnelfrp.com:10001", "--token", "tok"})
	if err == nil || !strings.Contains(err.Error(), "must start with http:// or https://") {
		t.Fatalf("run() error = %v, want --server scheme validation", err)
	}
}

func TestRunRejectsMissingServerURL(t *testing.T) {
	t.Setenv("GPUWORKS_STATE_DIR", t.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(log, []string{"--token", "tok"})
	if err == nil || !strings.Contains(err.Error(), "server URL is required") {
		t.Fatalf("run() error = %v, want --server required validation", err)
	}
}

func TestBenchSmokeAgainstHTTPFixture(t *testing.T) {
	tokenizer := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(tokenizer, []byte(`{"model":{"vocab":{"a":0,"b":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string                           `json:"model"`
			Messages  []struct{ Role, Content string } `json:"messages"`
			Stream    bool                             `json:"stream"`
			MaxTokens int                              `json:"max_tokens"`
			IgnoreEOS bool                             `json:"ignore_eos"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "fixture" || len(body.Messages) != 1 || body.Messages[0].Role != "user" || body.Messages[0].Content == "" || !body.Stream || body.MaxTokens != 1 || !body.IgnoreEOS {
			t.Error("CLI sent an invalid streaming completion request")
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	if err := bench([]string{"--url", srv.URL, "--model", "fixture", "--requests", "1", "--isl", "2", "--osl", "1", "--warmup-requests", "0", "--tokenizer", tokenizer}); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("inference requests=%d, want one measured request", requests.Load())
	}
}

func TestAgentLoggerPreservesRedirectedStderr(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS uses a launchd stderr file")
	}
	path := filepath.Join(t.TempDir(), "stderr.log")
	original := strings.Repeat("existing log\n", int(maxAgentLogBytes)/13+1)
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved; stderr.Close() })
	base := slog.New(slog.NewTextHandler(stderr, nil))
	state := t.TempDir()
	log, closeLog, err := agentLogger(base, state)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("redirected agent record")
	closeLog()
	for _, logPath := range []string{path, filepath.Join(state, "agent.log")} {
		data, err := os.ReadFile(logPath)
		if err != nil || !strings.Contains(string(data), "redirected agent record") {
			t.Fatalf("%s missing record: %v", logPath, err)
		}
		if logPath == path && !strings.HasPrefix(string(data), original) {
			t.Fatal("existing redirected stderr was truncated")
		}
	}
}
