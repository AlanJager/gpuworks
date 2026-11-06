package benchrun

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/tokenizer"
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
				t.Setenv("XDG_CACHE_HOME", t.TempDir())
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
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
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

func TestTokenizerFailureDiagnosticDistinguishesNetworkAndContent(t *testing.T) {
	for _, tt := range []struct {
		cause error
		want  string
	}{
		{&net.DNSError{Err: "temporary failure in name resolution", Name: "mirror.invalid", IsTemporary: true}, "DNS resolution failed"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, "network connection failed"},
		{fmt.Errorf("%w: HTTP 404 Not Found", tokenizer.ErrDownload), "tokenizer download failed"},
		{errors.New("tokenizer.json has no BPE vocab"), "tokenizer could not be loaded"},
		// A bare repository name read as a path: missing, not a network fault,
		// although ENOENT satisfies net.Error.
		{&fs.PathError{Op: "open", Path: "moonshotai/Kimi-K3.1-Pro", Err: syscall.ENOENT}, "tokenizer file not found: moonshotai/Kimi-K3.1-Pro"},
		{fmt.Errorf("%w: /v1/detokenize answered HTTP 404", tokenizer.ErrEngineTokenize), "choose another tokenizer source"},
	} {
		message := tokenizerLoadError(tt.cause).Error()
		if !strings.Contains(message, tt.want) || !strings.Contains(message, "evaluation was not started") {
			t.Fatalf("diagnostic=%q want=%q", message, tt.want)
		}
	}
}

type tokenizerRoundTrip func(*http.Request) (*http.Response, error)

func (f tokenizerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransientTokenizerDNSDoesNotFailBenchmark(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	endpoint := &fakeEndpoint{}
	server := endpoint.serve(t)
	original := http.DefaultClient
	defer func() { http.DefaultClient = original }()
	attempts := 0
	http.DefaultClient = &http.Client{Transport: tokenizerRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "tokenizer.invalid" {
			attempts++
			if attempts == 1 {
				return nil, &net.DNSError{Err: "temporary failure in name resolution", Name: r.URL.Host, IsTemporary: true}
			}
			return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"model":{"vocab":{"a":0,"b":1}}}`))}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	rec := &recorder{}
	Execute(context.Background(), Run{ID: 101, Model: "m", TokenizerRef: "https://tokenizer.invalid/tokenizer.json", Client: loadgen.Client{BaseURL: server.URL}, Dataset: json.RawMessage(`{"kind":"random","input_tokens":2,"output_tokens":3}`), Search: json.RawMessage(`{"concurrency":[1],"isl":[2],"osl":[3],"requests_per_point_multiplier":1}`)}, rec.hooks())
	if rec.last().State != "done" || attempts != 2 || len(endpoint.requests()) != 1 {
		t.Fatalf("terminal=%+v attempts=%d inference=%d", rec.last(), attempts, len(endpoint.requests()))
	}
}

func TestTokenizerWrappedTransportFailuresKeepDownloadClassification(t *testing.T) {
	for _, cause := range []error{io.EOF, tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}} {
		wrapped := fmt.Errorf("%w: %w", tokenizer.ErrDownload, &url.Error{Op: "Get", URL: "https://private-user:private-password@tokenizer.invalid/tokenizer.json?access_token=private-token", Err: cause})
		message := tokenizerLoadError(wrapped).Error()
		if !strings.Contains(message, "tokenizer download failed") || !strings.Contains(message, cause.Error()) {
			t.Fatalf("download misclassified: %s", message)
		}
		for _, secret := range []string{"private-user", "private-password", "private-token", "access_token"} {
			if strings.Contains(message, secret) {
				t.Fatalf("diagnostic exposed %s", secret)
			}
		}
	}
}

// A run whose reference names the target's tokenizer builds its prompts from
// the target's /v1/detokenize; when the target does not serve it, the
// fallback the reference names is loaded, and without a fallback the run
// stops before any inference.
func TestEngineTokenizerReferenceUsesTheTargetThenItsFallback(t *testing.T) {
	for _, tc := range []struct {
		name     string
		serve    bool
		fallback bool
		ok       bool
	}{
		{"the target serves tokenize", true, false, true},
		{"the target does not, a fallback is named", false, true, true},
		{"the target does not, no fallback", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenize, inference atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/tokenize", "/v1/detokenize":
					tokenize.Add(1)
					if !tc.serve {
						http.Error(w, "tokenize is turned off", http.StatusNotFound)
						return
					}
					var body struct {
						Tokens []int `json:"tokens"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if r.URL.Path == "/v1/tokenize" {
						_, _ = w.Write([]byte(`{"count":1,"max_model_len":4096,"tokens":[3],"token_strs":null}`))
						return
					}
					var b strings.Builder
					for _, id := range body.Tokens {
						if id >= 4000 {
							http.Error(w, `{"object":"error","message":"out of vocabulary"}`, http.StatusBadRequest)
							return
						}
						if id == 3 {
							b.WriteString("\n")
						} else {
							fmt.Fprintf(&b, " t%d", id)
						}
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"prompt": b.String()})
				default:
					inference.Add(1)
					http.Error(w, "stop after setup", http.StatusServiceUnavailable)
				}
			}))
			defer server.Close()
			ref := tokenizer.EngineRef
			if tc.fallback {
				ref += testTokenizerRef(t)
			}
			e := &execution{run: Run{ID: 5, Model: "m", TokenizerRef: ref, Client: loadgen.Client{BaseURL: server.URL + "/v1"}}, hooks: Hooks{Warn: func(string) {}}}
			e.dc.Kind = "random"
			err := e.loadTokenizer(context.Background())
			if (err == nil) != tc.ok {
				t.Fatalf("loadTokenizer err = %v, want ok=%v", err, tc.ok)
			}
			if tokenize.Load() == 0 {
				t.Fatal("the target's tokenizer was not asked")
			}
			if !tc.ok && !strings.Contains(err.Error(), "choose another tokenizer source") {
				t.Fatalf("err = %v", err)
			}
			if inference.Load() != 0 {
				t.Fatal("loading the tokenizer sent inference")
			}
		})
	}
}
