package benchrun

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func testTokenizerRef(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"a":0,"b":1,"c":2}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestControlledLengthsRequireTokenizerBeforeAnyInference(t *testing.T) {
	for _, kind := range []string{"random", "agentic"} {
		for _, source := range []string{"missing", "download failure", "invalid file"} {
			t.Run(kind+"/"+source, func(t *testing.T) {
				var inference atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/tokenizer.json" {
						if source == "download failure" {
							http.Error(w, "not found", 404)
						} else {
							_, _ = w.Write([]byte(`{"model":{"vocab":{}}}`))
						}
						return
					}
					inference.Add(1)
					http.Error(w, "unexpected inference", 500)
				}))
				defer server.Close()
				ref := ""
				if source != "missing" {
					ref = server.URL + "/tokenizer.json"
				}
				rec := &recorder{}
				Execute(context.Background(), Run{ID: 97, Mode: "sla", Model: "m", TokenizerRef: ref, Client: loadgen.Client{BaseURL: server.URL}, Dataset: json.RawMessage(`{"kind":"` + kind + `","input_tokens":4096}`), Search: json.RawMessage(`{"concurrency":1,"isl":[4096],"osl":[1],"warmup_requests":2,"requests_per_point_multiplier":1}`)}, rec.hooks())
				if last := rec.last(); last.State != "failed" || last.StopReason != proto.BenchmarkStopFatal || !strings.Contains(last.Message, "tokenizer") {
					t.Fatalf("terminal=%+v", last)
				}
				if inference.Load() != 0 || len(rec.points(t)) != 0 || len(rec.results) != 0 {
					t.Fatalf("invalid setup sent inference or published measurements: inference=%d points=%v", inference.Load(), rec.points(t))
				}
			})
		}
	}
}

func TestNaturalImportedWorkloadCanRunWithoutTokenizer(t *testing.T) {
	endpoint := &fakeEndpoint{}
	server := endpoint.serve(t)
	rec := &recorder{}
	Execute(context.Background(), Run{ID: 98, Model: "m", Client: loadgen.Client{BaseURL: server.URL}, Dataset: json.RawMessage(`{"kind":"imported","samples":[{"messages":[{"role":"user","content":"natural text"}],"output_tokens":3}]}`), Search: json.RawMessage(`{"concurrency":1,"isl":[8],"osl":[3],"requests_per_point_multiplier":1}`)}, rec.hooks())
	if rec.last().State != "done" || len(endpoint.requests()) != 1 {
		t.Fatalf("terminal=%+v requests=%d", rec.last(), len(endpoint.requests()))
	}
}

func TestTokenizerTransportFailureDoesNotPublishCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	server.Close()
	ref := strings.Replace(server.URL, "http://", "http://tokenizer-user:tokenizer-password@", 1) + "/tokenizer.json?access_token=tokenizer-query-secret"
	rec := &recorder{}
	Execute(context.Background(), Run{ID: 97, TokenizerRef: ref, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{}`)}, rec.hooks())
	message := rec.last().Message
	for _, secret := range []string{"tokenizer-user", "tokenizer-password", "tokenizer-query-secret", "access_token"} {
		if strings.Contains(message, secret) {
			t.Fatal("terminal tokenizer diagnostic leaked credentials")
		}
	}
	if rec.last().State != "failed" || !strings.Contains(message, "connection refused") {
		t.Fatalf("safe diagnostic lost useful cause: %q", message)
	}
}

func TestTokenizerCancellationPreservesContextSemantics(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := &execution{run: Run{TokenizerRef: "http://example.invalid/tokenizer.json"}, hooks: Hooks{Warn: func(string) {}}}
	if err := e.loadTokenizer(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	rec := &recorder{}
	Execute(ctx, e.run, rec.hooks())
	if len(rec.statuses) != 0 {
		t.Fatal("cancellation published a fatal tokenizer status")
	}
}

func TestTokenizerNestedTransportErrorsAreSafeAndKeepCancellation(t *testing.T) {
	for _, cause := range []error{errors.New("transport failed"), context.Canceled, context.DeadlineExceeded} {
		inner := &url.Error{Op: "Get", URL: "http://nested-user:nested-pass@host/tokenizer?token=nested-secret", Err: cause}
		outer := &url.Error{Op: "Get", URL: "http://outer-user:outer-pass@host/?key=outer-secret", Err: inner}
		safe := tokenizerLoadError(outer)
		for _, secret := range []string{"nested-user", "nested-pass", "nested-secret", "outer-user", "outer-pass", "outer-secret"} {
			if strings.Contains(safe.Error(), secret) {
				t.Fatal("nested tokenizer transport error exposed credentials")
			}
		}
		if (cause == context.Canceled || cause == context.DeadlineExceeded) && !errors.Is(safe, cause) {
			t.Fatalf("lost sentinel %v", cause)
		}
	}
}
