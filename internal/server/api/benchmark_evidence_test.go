package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/bench/dataset"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

// Exercise the real server-origin execution, persistence, API projection,
// JSONL and all report formats against a streaming OpenAI-compatible fixture.
func TestBenchmarkEvidenceServerExecutionToReport(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req loadgen.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Messages) == 0 || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			http.Error(w, "invalid chat request", 400)
			return
		}
		if req.Messages[0].Content == "fail" {
			http.Error(w, `{"error":{"message":"context rejected","type":"invalid_request_error"}}`, 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n")
		w.(http.Flusher).Flush()
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":0}}}\n\ndata: [DONE]\n\n")
	}))
	defer endpoint.Close()
	st, err := store.Open("file:benchmark_evidence_execution?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	samples := make([]dataset.Sample, 4)
	for i := range samples {
		samples[i] = dataset.Sample{Messages: []dataset.Message{{Role: "user", Content: "ok"}}, OutputTokens: 220, FollowUps: make([]dataset.Message, 12)}
		for j := range samples[i].FollowUps {
			samples[i].FollowUps[j] = dataset.Message{Role: "user", Content: "next"}
		}
	}
	samples[3].Messages[0].Content = "fail"
	dc, _ := json.Marshal(dataset.Config{Kind: "imported", Seed: 42, Samples: samples})
	run, err := st.CreateBenchmarkRun(&store.BenchmarkRun{Name: "Evidence execution", TargetKind: "external", BaseURL: endpoint.URL, Model: "m", LoadOrigin: "server", Mode: "grid", Dataset: string(dc), Search: `{"concurrency":[1],"isl":[8],"osl":[220],"sessions_per_point":{"1":4},"warmup_requests":0}`, KeepDetail: true, DetailCapMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	handlers := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, Bus: bus.New(), BenchmarkArtifactDir: dir}}
	handlers.runServerBenchmark(context.Background(), run)
	points, err := st.ListBenchmarkPoints(run.ID)
	if err != nil || len(points) != 1 {
		t.Fatalf("points=%+v err=%v", points, err)
	}
	point := points[0]
	if point.Success != 39 || point.Errors != 1 || point.DetailPath == "" {
		t.Fatalf("point=%+v", point)
	}
	signer, _ := auth.NewTokenSigner("benchmark-evidence", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, Bus: bus.New(), AuthSigner: signer, BenchmarkArtifactDir: dir})
	base := "/api/v1/benchmarks/" + strconv.FormatUint(uint64(run.ID), 10)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", path, w.Code, w.Body.String())
		}
		return w
	}
	response := get(base)
	var wire struct {
		Points []struct {
			LongTail struct {
				Evidence struct {
					Version int `json:"version"`
					Plan    struct {
						Sessions int `json:"planned_sessions"`
						Requests int `json:"planned_requests"`
					} `json:"plan"`
					Outcomes struct {
						Completed int `json:"completed_sessions"`
						Skipped   int `json:"skipped"`
					} `json:"outcomes"`
					All struct {
						Attempted int `json:"attempted"`
						Success   int `json:"success"`
						Failed    int `json:"failed"`
						Cache     struct {
							Reported int `json:"reported"`
							Unknown  int `json:"unknown"`
						} `json:"cache"`
					} `json:"all"`
				} `json:"evidence"`
			} `json:"long_tail"`
		} `json:"points"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Points) != 1 {
		t.Fatal(response.Body.String())
	}
	e := wire.Points[0].LongTail.Evidence
	if e.Version != 1 || e.Plan.Sessions != 4 || e.Plan.Requests != 52 || e.All.Attempted != 40 || e.All.Success != 39 || e.All.Failed != 1 || e.Outcomes.Skipped != 12 || e.Outcomes.Completed != 3 || e.All.Cache.Reported != 39 || e.All.Cache.Unknown != 1 {
		t.Fatal(response.Body.String())
	}
	for _, format := range []string{"csv", "markdown", "html"} {
		report := get(base + "/report?format=" + format).Body.String()
		if format == "csv" {
			records, err := csv.NewReader(strings.NewReader(report)).ReadAll()
			if err != nil || len(records) != 2 {
				t.Fatalf("csv=%q err=%v", report, err)
			}
			columns := map[string]string{}
			for i, key := range records[0] {
				columns[key] = records[1][i]
			}
			for key, want := range map[string]string{"planned_requests": "52", "requests": "40", "successful_requests": "39", "failed_requests": "1", "skipped_requests": "12", "session_completion_rate": "75.00%", "detail_status": "complete"} {
				if columns[key] != want {
					t.Fatalf("column %s=%q want=%q", key, columns[key], want)
				}
			}
		} else if !strings.Contains(report, "session_completion_rate") || !strings.Contains(report, "75.00%") || !strings.Contains(report, "context rejected") {
			t.Fatalf("report %s missing evidence: %s", format, report)
		}
	}
	file, err := os.Open(point.DetailPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	scanner := bufio.NewScanner(gz)
	count, failures := 0, 0
	for scanner.Scan() {
		var d loadgen.DetailRecord
		if err := json.Unmarshal(scanner.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		count++
		if d.Version != 1 || d.RunID != run.ID || d.PointSeq != 1 || d.SessionID == "" || d.Turn < 1 || d.PlannedTurns != 13 || d.Metrics.StartedAt == nil || d.Metrics.FinishedAt == nil {
			t.Fatalf("detail=%s", scanner.Bytes())
		}
		if d.ErrorText != "" {
			failures++
			if d.HTTPStatus != 400 || !strings.Contains(d.ErrorText, "context rejected") {
				t.Fatalf("error detail=%s", scanner.Bytes())
			}
		}
	}
	if scanner.Err() != nil || count != 40 || failures != 1 {
		t.Fatalf("records=%d failures=%d err=%v", count, failures, scanner.Err())
	}
}

func TestBenchmarkReportDetailReaderMarksMissingTruncatedAndForeignFiles(t *testing.T) {
	root := t.TempDir()
	run := &store.BenchmarkRun{ID: 9, KeepDetail: true, DetailCapMB: 1}
	h := &apiHandlers{d: Deps{BenchmarkArtifactDir: root}}
	row := loadgen.DetailRecord{Version: 1, RunID: 9, PointSeq: 1, SessionID: "session-1", Turn: 1, PlannedTurns: 1, Attempt: 1}
	write := func(path string, lines ...any) {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		gz := gzip.NewWriter(f)
		for _, line := range lines {
			if err := json.NewEncoder(gz).Encode(line); err != nil {
				t.Fatal(err)
			}
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "detail.jsonl.gz")
	point := store.BenchmarkPoint{Seq: 1, DetailPath: path}
	if _, status, _ := h.readBenchmarkReportDetails(context.Background(), run, point); status != "unavailable" {
		t.Fatal(status)
	}
	write(path, row, struct {
		Truncated bool `json:"truncated"`
	}{true})
	if rows, status, _ := h.readBenchmarkReportDetails(context.Background(), run, point); status != "truncated" || len(rows) != 1 {
		t.Fatalf("rows=%+v status=%s", rows, status)
	}
	outside := filepath.Join(t.TempDir(), "foreign.jsonl.gz")
	write(outside, row)
	link := filepath.Join(root, "foreign-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	point.DetailPath = link
	if _, status, _ := h.readBenchmarkReportDetails(context.Background(), run, point); status != "unavailable" {
		t.Fatal(status)
	}
	point.DetailPath = path
	write(path, map[string]any{"metrics": map[string]any{}, "text": "legacy"})
	if _, status, _ := h.readBenchmarkReportDetails(context.Background(), run, point); status != "unavailable" {
		t.Fatal(status)
	}
	run.KeepDetail = false
	if _, status, _ := h.readBenchmarkReportDetails(context.Background(), run, point); status != "disabled" {
		t.Fatal(status)
	}
}

func TestBenchmarkReportDetailBudgetIsSharedAcrossPoints(t *testing.T) {
	root := t.TempDir()
	run := &store.BenchmarkRun{ID: 12, KeepDetail: true, DetailCapMB: 1}
	handlers := &apiHandlers{d: Deps{BenchmarkArtifactDir: root}}
	for seq := 1; seq <= 2; seq++ {
		detail, err := newBenchmarkDetail(root, run.ID, seq, true, 1)
		if err != nil {
			t.Fatal(err)
		}
		detail.Add(loadgen.Result{Metrics: metrics.Request{SessionID: "session-1", Turn: 1, PlannedTurns: 1}})
		if _, err := detail.Close(); err != nil {
			t.Fatal(err)
		}
	}
	budget := &benchmarkReportDetailBudget{bytes: 1 << 20, records: 1}
	first := store.BenchmarkPoint{Seq: 1, DetailPath: filepath.Join(root, "run-12-point-1.jsonl.gz")}
	if rows, status, _ := handlers.readBenchmarkReportDetailsWithin(context.Background(), run, first, budget); len(rows) != 1 || status != "complete" {
		t.Fatalf("rows=%d status=%s", len(rows), status)
	}
	second := store.BenchmarkPoint{Seq: 2, DetailPath: filepath.Join(root, "run-12-point-2.jsonl.gz")}
	if rows, status, note := handlers.readBenchmarkReportDetailsWithin(context.Background(), run, second, budget); len(rows) != 0 || status != "truncated" || !strings.Contains(note, "shared") {
		t.Fatalf("rows=%d status=%s note=%s", len(rows), status, note)
	}
}
