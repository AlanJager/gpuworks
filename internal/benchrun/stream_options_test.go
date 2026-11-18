package benchrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestRunStreamOptionsFollowTheRequestedResponseMode(t *testing.T) {
	for _, noStream := range []bool{true, false} {
		for _, reportUsage := range []bool{true, false} {
			t.Run(fmt.Sprintf("no-stream=%t/usage=%t", noStream, reportUsage), func(t *testing.T) {
				var requests atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unsupported endpoint", http.StatusNotFound)
						return
					}
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "invalid JSON", http.StatusBadRequest)
						return
					}
					requests.Add(1)
					stream := string(body["stream"]) == "true"
					if !stream && len(body["stream_options"]) != 0 && string(body["stream_options"]) != "null" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_, _ = w.Write([]byte(`{"error":{"message":"Stream options can only be defined when stream=True.","type":"BadRequestError","param":"stream_options","code":400}}`))
						return
					}
					if stream != !noStream || string(body["model"]) != `"m"` || string(body["max_tokens"]) != "3" || body["ignore_eos"] != nil {
						t.Errorf("unexpected completion parameters: %s", body)
						http.Error(w, "invalid parameters", http.StatusBadRequest)
						return
					}
					if stream && string(body["stream_options"]) != `{"include_usage":true}` {
						t.Errorf("unexpected stream_options on actual wire: %s", body["stream_options"])
						http.Error(w, "invalid stream options", http.StatusBadRequest)
						return
					}
					if !stream && body["stream_options"] != nil {
						t.Errorf("non-streaming request must omit stream_options: %s", body["stream_options"])
					}
					usage := `{"prompt_tokens":8,"completion_tokens":3,"total_tokens":11}`
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"abc\"},\"finish_reason\":\"stop\"}]}\n\n"))
						if reportUsage {
							_, _ = fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\n", usage)
						}
						_, _ = w.Write([]byte("data: [DONE]\n\n"))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if reportUsage {
						_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":"abc"},"finish_reason":"stop"}],"usage":%s}`, usage)
					} else {
						_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"abc"},"finish_reason":"stop"}]}`))
					}
				}))
				t.Cleanup(srv.Close)
				rec := &recorder{}
				hooks := rec.hooks()
				var measured []loadgen.Result
				hooks.PointReported = func(_ int, results []loadgen.Result) { measured = results }
				Execute(context.Background(), Run{ID: 330, Model: "m", NoStream: noStream,
					TokenizerRef: testTokenizerRef(t), Client: loadgen.Client{BaseURL: srv.URL},
					Dataset: json.RawMessage(`{"kind":"random","seed":42}`),
					Search:  json.RawMessage(`{"concurrency":[1],"isl":[8],"osl":[3],"warmup_requests":1,"requests_per_point_multiplier":1}`),
				}, hooks)
				if done := rec.last(); done.State != "done" {
					t.Fatalf("strict endpoint rejected the benchmark: state=%s message=%s", done.State, done.Message)
				}
				points := rec.points(t)
				if len(points) != 1 || points[0].Success != 1 || points[0].Errors != 0 || len(measured) != 1 || requests.Load() != 2 {
					t.Fatalf("points=%+v measured=%d HTTP requests=%d; want one warmup and one successful measured request", points, len(measured), requests.Load())
				}
				result := measured[0]
				m := result.Metrics
				if result.Stream != !noStream || m.HTTPStatus != http.StatusOK || m.Error != nil {
					t.Fatalf("measured response = %+v", result)
				}
				if reportUsage {
					if m.ServerInputTokens == nil || *m.ServerInputTokens != 8 || m.ServerOutputTokens == nil || *m.ServerOutputTokens != 3 || m.ApproximateTokens || m.InputSource != "server_usage" || m.OutputSource != "server_usage" {
						t.Fatalf("server usage was not preserved: %+v", m)
					}
				} else if m.ServerInputTokens != nil || m.ServerOutputTokens != nil || !m.ApproximateTokens || m.InputSource == "server_usage" || m.OutputSource == "server_usage" || len(result.UsageJSON) != 0 {
					t.Fatalf("missing usage became reported server values: %+v", result)
				}
				if m.CachedTokensAvailable || m.ReasoningTokens != nil {
					t.Fatalf("missing optional usage became observed values: %+v", m)
				}
				if noStream && (!m.FirstOutputAt.IsZero() || m.TTFT != 0 || m.TPOT != 0 || m.Chunks != 0) {
					t.Fatalf("non-streaming response invented stream timings: %+v", m)
				}
			})
		}
	}
}
