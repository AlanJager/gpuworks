package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestCanceledBenchmarkCSVRetainsReportContext(t *testing.T) {
	started := make(chan struct{}, 1)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fixture.Close)
	st, err := store.Open("file:benchmark_cancel_report_context?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, _ := auth.NewTokenSigner("benchmark-cancel-report-context", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	handler := Router(Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), AuthSigner: signer})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	body := fmt.Sprintf(`{"name":"cancel-report-context","target_kind":"external","base_url":%q,"model":"test-model","load_origin":"server","tokenizer_source":"upload","tokenizer_ref":%q,"dataset":{"kind":"random","input_tokens":8,"output_tokens":4,"seed":42},"mode":"grid","search":{"concurrency":1,"isl":[8],"osl":[4],"requests_per_point_multiplier":3,"warmup_requests":0}}`, fixture.URL, benchmarkTestTokenizer(t))
	created := call(http.MethodPost, "/api/v1/benchmarks", body)
	if created.Code != http.StatusAccepted {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var run benchmarkDTO
	if err := json.Unmarshal(created.Body.Bytes(), &run); err != nil || run.ID == 0 {
		t.Fatalf("create run: %v", err)
	}
	// Ensure failed assertions also stop the live executor before the fixture closes.
	t.Cleanup(func() { _ = call(http.MethodPost, fmt.Sprintf("/api/v1/benchmarks/%d/cancel", run.ID), "") })
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("benchmark did not begin")
	}
	base := fmt.Sprintf("/api/v1/benchmarks/%d", run.ID)
	canceled := call(http.MethodPost, base+"/cancel", "")
	if canceled.Code != http.StatusOK {
		t.Fatalf("cancel: %d %s", canceled.Code, canceled.Body.String())
	}
	persisted, err := st.GetBenchmarkRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.State != "canceled" || persisted.Verdict != "" {
		t.Fatalf("state=%q verdict=%q; cancellation must retain unknown verdict", persisted.State, persisted.Verdict)
	}
	exported := call(http.MethodGet, base+"/report?format=csv", "")
	if exported.Code != http.StatusOK {
		t.Fatalf("report: %d %s", exported.Code, exported.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(exported.Body.String())).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatalf("CSV rows=%d err=%v", len(rows), err)
	}
	columns := map[string]string{}
	for i, key := range rows[0] {
		columns[key] = rows[1][i]
	}
	if columns["point_state"] != "canceled" {
		t.Fatalf("point state=%q", columns["point_state"])
	}
	var context map[string]json.RawMessage
	if err := json.Unmarshal([]byte(columns["report_context_json"]), &context); err != nil {
		t.Fatalf("context JSON invalid: %v", err)
	}
	for key, want := range map[string]string{"run_id": fmt.Sprint(run.ID), "model": `"test-model"`, "persisted_verdict": "null"} {
		if string(context[key]) != want {
			t.Fatalf("context %s=%s want %s", key, context[key], want)
		}
	}
	var dataset, search, snapshot map[string]any
	if err := json.Unmarshal(context["dataset"], &dataset); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(context["search"], &search); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(context["target_snapshot"], &snapshot); err != nil {
		t.Fatal(err)
	}
	if dataset["kind"] != "random" || dataset["input_tokens"] != float64(8) || dataset["output_tokens"] != float64(4) || dataset["seed"] != float64(42) {
		t.Fatalf("dataset=%v", dataset)
	}
	if search["concurrency"] != float64(1) || search["warmup_requests"] != float64(0) || search["requests_per_point_multiplier"] != float64(3) {
		t.Fatalf("search=%v", search)
	}
	if snapshot["target_kind"] != "external" || snapshot["base_url"] != fixture.URL || snapshot["model"] != "test-model" || snapshot["load_origin"] != "server" {
		t.Fatalf("snapshot=%v", snapshot)
	}
}
