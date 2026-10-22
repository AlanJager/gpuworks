package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestBenchmarkReportDownloadAPI(t *testing.T) {
	st, err := store.Open("file:benchmark_report_api?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{
		Name: "Agentic 80K", State: "done", Mode: "grid", Model: "glm-5.2",
		Dataset: `{"kind":"agentic","turns":13,"mean_input_tokens":79998,"output_tokens":220,"design_prefix_reuse_rate":0.92,"workload_fingerprint":"agentic-80k-v1-test"}`,
		Search:  `{"sessions_per_point":{"4":8}}`, TargetSnapshot: `{"engine":"sglang","env":{"API_KEY":"secret"}}`, Warnings: `["BENCHMARK_AGENTIC_LENGTH_DRIFT"]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBenchmarkPoint(&store.BenchmarkPoint{RunID: run.ID, Seq: 1, State: "done", Concurrency: 4, Success: 104, ActualISL: 80001, ActualOSL: 220, OutputTput: 400, TotalTput: 500, RequestTput: 3, P50TTFT: 20, P99TTFT: 40, P50TPOT: 4, P99TPOT: 7, LongTail: `{"KVHitRate":0.91,"CacheReportCoverage":1,"PeakInputTokens":85800}`}); err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("benchmark-report-api", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	handler := Router(Deps{Store: st, Bus: bus.New(), AuthSigner: signer})
	basePath := "/api/v1/benchmarks/" + strconv.FormatUint(uint64(run.ID), 10)
	path := basePath + "/report"

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, path+"?format=csv", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	for _, tc := range []struct {
		format, contentType, extension, contains string
	}{
		{"markdown", "text/markdown", ".md", "actual_peak_isl"},
		{"csv", "text/csv", ".csv", "interactivity_tok_s_user"},
		{"html", "text/html", ".html", "<svg"},
	} {
		req := httptest.NewRequest(http.MethodGet, path+"?format="+tc.format, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(rec.Header().Get("Content-Disposition"), tc.extension) || !strings.Contains(rec.Body.String(), tc.contains) {
			t.Fatalf("format=%s status=%d headers=%v body=%s", tc.format, rec.Code, rec.Header(), rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("format=%s leaked sensitive environment value", tc.format)
		}
		if tc.format != "csv" && !strings.Contains(rec.Body.String(), "/evaluations/"+strconv.FormatUint(uint64(run.ID), 10)) {
			t.Fatalf("format=%s is missing the evaluation link: %s", tc.format, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "BENCHMARK_AGENTIC_LENGTH_DRIFT") {
			t.Fatalf("format=%s retained a legacy warning below the current threshold", tc.format)
		}
	}

	detailReq := httptest.NewRequest(http.MethodGet, basePath, nil)
	detailReq.Header.Set("Authorization", "Bearer "+token)
	detailRec := httptest.NewRecorder()
	handler.ServeHTTP(detailRec, detailReq)
	if detailRec.Code != http.StatusOK || strings.Contains(detailRec.Body.String(), "BENCHMARK_AGENTIC_LENGTH_DRIFT") {
		t.Fatalf("detail retained a legacy warning below the current threshold: status=%d body=%s", detailRec.Code, detailRec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, path+"?format=pdf", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid format status=%d body=%s", rec.Code, rec.Body.String())
	}
}
