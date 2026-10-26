package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func benchmarkTestTokenizer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tokenizer.json")
	if err := os.WriteFile(path, []byte(`{"model":{"vocab":{"a":0,"b":1}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestServerBenchmarkPersistsTokenizerFailureWithoutMeasurements(t *testing.T) {
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(fixture.Close)
	st, err := store.Open("file:benchmark_tokenizer_warning?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{
		Name: "tokenizer warning", BaseURL: fixture.URL, Model: "m", LoadOrigin: "server",
		Dataset: `{"kind":"random","input_tokens":2,"output_tokens":1}`,
		Mode:    "grid", Search: `{"concurrency":[1],"isl":[2],"osl":[1],"requests_per_point_multiplier":1}`,
		TokenizerRef: filepath.Join(t.TempDir(), "missing-tokenizer.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st}}
	h.runServerBenchmark(context.Background(), run)
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "failed" || !strings.Contains(persisted.Error, "tokenizer") || !strings.Contains(persisted.Verdict, `"kind":"failed"`) || !strings.Contains(persisted.Warnings, proto.BenchmarkWarningTokenizerLoad) {
		t.Fatalf("run after tokenizer failure = %+v", persisted)
	}
	points, err := st.ListBenchmarkPoints(run.ID)
	if err != nil || len(points) != 0 {
		t.Fatalf("server-origin curve points=%+v err=%v", points, err)
	}
}

func TestTokenizerTransportFailurePersistsOnlySafeDiagnostic(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	endpoint.Close()
	ref := strings.Replace(endpoint.URL, "http://", "http://tokenizer-user:tokenizer-password@", 1) + "/tokenizer.json?access_token=tokenizer-query-secret"
	st, err := store.Open("file:benchmark_tokenizer_safe_error?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "safe tokenizer failure", Model: "m", LoadOrigin: "server", TokenizerRef: ref, Dataset: `{"kind":"random"}`, Search: `{}`, Mode: "sla"})
	if err != nil {
		t.Fatal(err)
	}
	handlers := &apiHandlers{d: Deps{Store: st}}
	handlers.runServerBenchmark(context.Background(), run)
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("tokenizer-safe-error", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, Bus: bus.New(), AuthSigner: signer})
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/benchmarks/%d", run.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusOK {
		t.Fatalf("detail status=%d", response.Code)
	}
	var detail struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{persisted.Error, detail.Error} {
		for _, secret := range []string{"tokenizer-user", "tokenizer-password", "tokenizer-query-secret", "access_token"} {
			if strings.Contains(text, secret) {
				t.Fatal("tokenizer diagnostic exposed credentials")
			}
		}
		if !strings.Contains(text, "connection refused") {
			t.Fatalf("missing useful safe cause: %q", text)
		}
	}
	if persisted.State != "failed" || !strings.Contains(persisted.Verdict, `"kind":"failed"`) {
		t.Fatalf("state=%q verdict=%q", persisted.State, persisted.Verdict)
	}
}
