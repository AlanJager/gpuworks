package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSharedHandlerOpenAICompatibleOperations(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()

	for _, request := range []struct {
		name        string
		path        string
		body        string
		contentType string
		wantType    string
		wantBody    string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"fake-model","messages":[]}`, contentType: "application/json", wantType: "application/json", wantBody: `"object":"chat.completion"`},
		{name: "embeddings", path: "/v1/embeddings", body: `{"model":"fake-model","input":"hello"}`, contentType: "application/json", wantType: "application/json", wantBody: `"object":"embedding"`},
		{name: "rerank", path: "/v1/rerank", body: `{"model":"fake-model","query":"hello","documents":["one","two"]}`, contentType: "application/json", wantType: "application/json", wantBody: `"relevance_score"`},
		{name: "speech", path: "/v1/audio/speech", body: `{"model":"fake-model","input":"hello","voice":"alloy","response_format":"wav"}`, contentType: "application/json", wantType: "audio/wav", wantBody: "RIFF"},
		{name: "image", path: "/v1/images/generations", body: `{"model":"fake-model","prompt":"mountains","size":"1024x1024"}`, contentType: "application/json", wantType: "application/json", wantBody: `"b64_json"`},
	} {
		t.Run(request.name, func(t *testing.T) {
			response := post(t, server.URL+request.path, request.contentType, strings.NewReader(request.body))
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), request.wantType) || !bytes.Contains(body, []byte(request.wantBody)) {
				t.Fatalf("status=%d type=%q body=%q", response.StatusCode, response.Header.Get("Content-Type"), body)
			}
		})
	}

	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("model", "fake-model")
	file, err := writer.CreateFormFile("file", "audio.wav")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("RIFF-audio"))
	_ = writer.Close()
	response := post(t, server.URL+"/v1/audio/transcriptions", writer.FormDataContentType(), &form)
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"text":"hello from fake-engine"`)) {
		t.Fatalf("transcription status=%d body=%s", response.StatusCode, body)
	}
}

func TestSharedHandlerChatStreamAndSleepWake(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()

	stream := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[],"stream":true}`))
	streamBody, _ := io.ReadAll(stream.Body)
	stream.Body.Close()
	if stream.StatusCode != http.StatusOK || stream.Header.Get("Content-Type") != "text/event-stream" || !strings.Contains(string(streamBody), "data: [DONE]") {
		t.Fatalf("stream status=%d type=%q body=%q", stream.StatusCode, stream.Header.Get("Content-Type"), streamBody)
	}
	response := post(t, server.URL+"/sleep", "", strings.NewReader(""))
	response.Body.Close()
	response, err := http.Get(server.URL + "/is_sleeping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "{\"is_sleeping\":true}\n" {
		t.Fatalf("sleep response=%q", body)
	}
	response = post(t, server.URL+"/wake_up", "", strings.NewReader(""))
	response.Body.Close()
}

func TestSharedHandlerModelsPartialWakeAndLateSleepCompletion(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model", PartialWakeFailures: 1, SleepDelay: 15 * time.Millisecond}))
	defer server.Close()

	response := post(t, server.URL+"/sleep?level=2", "", strings.NewReader(""))
	response.Body.Close()
	response = post(t, server.URL+"/wake_up?tags=weights", "", strings.NewReader(""))
	response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("partial wake status=%d, want 500", response.StatusCode)
	}
	awake, err := http.Get(server.URL + "/is_sleeping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(awake.Body)
	awake.Body.Close()
	if string(body) != "{\"is_sleeping\":true}\n" {
		t.Fatalf("partial wake did not expose the side effect: %q", body)
	}
	started := time.Now()
	response = post(t, server.URL+"/sleep?level=2", "", strings.NewReader(""))
	response.Body.Close()
	if response.StatusCode != http.StatusOK || time.Since(started) < 15*time.Millisecond {
		t.Fatalf("delayed sleep status=%d elapsed=%s", response.StatusCode, time.Since(started))
	}
}

// TestSharedHandlerStatusOverrideForcesChatCompletionsStatus proves the
// /debug/status-override runtime toggle (an L3 fixture's way to make a real,
// already-running instance answer with an arbitrary HTTP status, e.g. to
// exercise the proxy's 429/5xx accounting classes without killing the
// container): unset it answers normally, POSTing a code forces every
// subsequent /v1/chat/completions response to that status until cleared
// with code=0.
func TestSharedHandlerStatusOverrideForcesChatCompletionsStatus(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()

	normal := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
	normal.Body.Close()
	if normal.StatusCode != http.StatusOK {
		t.Fatalf("status before any override = %d, want 200", normal.StatusCode)
	}

	set := post(t, server.URL+"/debug/status-override?code=429", "", strings.NewReader(""))
	setBody, _ := io.ReadAll(set.Body)
	set.Body.Close()
	if set.StatusCode != http.StatusOK || string(setBody) != "{\"status_override\":429}\n" {
		t.Fatalf("set override status=%d body=%q", set.StatusCode, setBody)
	}

	forced := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
	forcedBody, _ := io.ReadAll(forced.Body)
	forced.Body.Close()
	if forced.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(forcedBody), "forced status override") {
		t.Fatalf("forced status=%d body=%q, want 429 with the override message", forced.StatusCode, forcedBody)
	}

	get, err := http.Get(server.URL + "/debug/status-override")
	if err != nil {
		t.Fatal(err)
	}
	getBody, _ := io.ReadAll(get.Body)
	get.Body.Close()
	if string(getBody) != "{\"status_override\":429}\n" {
		t.Fatalf("GET override body=%q, want the still-set 429", getBody)
	}

	cleared := post(t, server.URL+"/debug/status-override?code=0", "", strings.NewReader(""))
	cleared.Body.Close()
	restored := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
	restored.Body.Close()
	if restored.StatusCode != http.StatusOK {
		t.Fatalf("status after clearing the override = %d, want 200", restored.StatusCode)
	}
}

func TestSharedHandlerAsyncVideoLifecycle(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()

	created := post(t, server.URL+"/v1/videos", "application/json", strings.NewReader(`{"model":"fake-model","prompt":"sunrise"}`))
	if created.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(created.Body)
		created.Body.Close()
		t.Fatalf("create status=%d body=%s", created.StatusCode, body)
	}
	var job videoJob
	if err := json.NewDecoder(created.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if job.ID == "" || job.Status != "queued" {
		t.Fatalf("created job=%+v", job)
	}

	status, err := http.Get(server.URL + "/v1/videos/" + job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(status.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	status.Body.Close()
	if job.Status != "completed" {
		t.Fatalf("polled job=%+v", job)
	}
	content, err := http.Get(server.URL + "/v1/videos/" + job.ID + "/content")
	if err != nil {
		t.Fatal(err)
	}
	video, _ := io.ReadAll(content.Body)
	content.Body.Close()
	if content.StatusCode != http.StatusOK || content.Header.Get("Content-Type") != "video/mp4" || !bytes.Contains(video, []byte("ftypisom")) {
		t.Fatalf("content status=%d type=%q body=%q", content.StatusCode, content.Header.Get("Content-Type"), video)
	}
	cancelRequest, _ := http.NewRequest(http.MethodDelete, server.URL+"/v1/videos/"+job.ID, nil)
	cancelled, err := http.DefaultClient.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(cancelled.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	cancelled.Body.Close()
	if job.Status != "deleted" {
		t.Fatalf("cancelled job=%+v", job)
	}
}

func TestSharedHandlerKeepsLongVideoQueuedForCancellation(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()
	created := post(t, server.URL+"/v1/videos", "application/json", strings.NewReader(`{"model":"fake-model","prompt":"sunrise","seconds":60}`))
	var job videoJob
	if err := json.NewDecoder(created.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	status, err := http.Get(server.URL + "/v1/videos/" + job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(status.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	status.Body.Close()
	if job.Status != "queued" {
		t.Fatalf("first long-video poll=%+v, want queued", job)
	}
	cancelRequest, _ := http.NewRequest(http.MethodDelete, server.URL+"/v1/videos/"+job.ID, nil)
	cancelled, err := http.DefaultClient.Do(cancelRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(cancelled.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	cancelled.Body.Close()
	if job.Status != "deleted" {
		t.Fatalf("cancelled long-video job=%+v", job)
	}
}

func TestSharedHandlerExposesTerminalVideoFailure(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()
	created := post(t, server.URL+"/v1/videos", "application/json", strings.NewReader(`{"model":"fake-model","prompt":"fail requested"}`))
	var job videoJob
	if err := json.NewDecoder(created.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if job.ID == "" || job.Status != "failed" {
		t.Fatalf("created failed job=%+v", job)
	}
	status, err := http.Get(server.URL + "/v1/videos/" + job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(status.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	status.Body.Close()
	if job.Status != "failed" {
		t.Fatalf("polled failed job=%+v", job)
	}
}

func TestSharedHandlerValidatesMethodsEncodingsAndRequiredFields(t *testing.T) {
	server := httptest.NewServer(New(Options{Model: "fake-model"}))
	defer server.Close()

	for _, request := range []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		wantStatus  int
		wantMessage string
	}{
		{name: "wrong method", method: http.MethodGet, path: "/v1/chat/completions", wantStatus: http.StatusMethodNotAllowed, wantMessage: "method is not allowed"},
		{name: "wrong encoding", method: http.MethodPost, path: "/v1/embeddings", contentType: "text/plain", body: `{}`, wantStatus: http.StatusBadRequest, wantMessage: "application/json is required"},
		{name: "missing embeddings input", method: http.MethodPost, path: "/v1/embeddings", contentType: "application/json", body: `{}`, wantStatus: http.StatusBadRequest, wantMessage: "input is required"},
		{name: "missing rerank documents", method: http.MethodPost, path: "/v1/rerank", contentType: "application/json", body: `{"query":"hello"}`, wantStatus: http.StatusBadRequest, wantMessage: "query and documents are required"},
		{name: "transcription not multipart", method: http.MethodPost, path: "/v1/audio/transcriptions", contentType: "application/json", body: `{}`, wantStatus: http.StatusBadRequest, wantMessage: "multipart/form-data is required"},
		{name: "unknown video", method: http.MethodGet, path: "/v1/videos/missing", wantStatus: http.StatusNotFound, wantMessage: "video task was not found"},
	} {
		t.Run(request.name, func(t *testing.T) {
			httpRequest, _ := http.NewRequest(request.method, server.URL+request.path, strings.NewReader(request.body))
			if request.contentType != "" {
				httpRequest.Header.Set("Content-Type", request.contentType)
			}
			response, err := http.DefaultClient.Do(httpRequest)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != request.wantStatus || !bytes.Contains(body, []byte(request.wantMessage)) || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("status=%d type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
			}
		})
	}
}

// TestSharedHandlerMetricsMirrorsVLLMDialect checks that registerMetrics'
// deterministic per-scrape exposition (see metrics_test.go) folds in live
// /v1/chat/completions traffic on top of its static baseline: token counters
// and the running/waiting gauges move by exactly the request's real
// contribution, in addition to the fixed per-scrape amount every scrape adds
// regardless of traffic (PromptTokens/CompletionTokens with no other Metrics*
// fields set, so the per-scrape baseline is exactly those two values).
func TestSharedHandlerMetricsMirrorsVLLMDialect(t *testing.T) {
	var logs []string
	server := httptest.NewServer(New(Options{Model: "fake-model", PromptTokens: 16, CompletionTokens: 8, Logf: func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}}))
	defer server.Close()

	// Idle: first scrape, no real traffic yet — exactly the static per-scrape
	// baseline (1 scrape * PromptTokens/CompletionTokens), gauges at 0, in the
	// vllm: namespace the agent's collector maps.
	resp, err := http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	idle, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("metrics status=%d type=%q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		`vllm:num_requests_running{model_name="fake-model"} 0`,
		`vllm:num_requests_waiting{model_name="fake-model"} 0`,
		`vllm:prompt_tokens_total{model_name="fake-model"} 16`,
		`vllm:generation_tokens_total{model_name="fake-model"} 8`,
		`vllm:kv_cache_usage_perc{model_name="fake-model"}`,
	} {
		if !strings.Contains(string(idle), want) {
			t.Fatalf("idle exposition missing %q:\n%s", want, idle)
		}
	}

	response := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("chat status=%d", response.StatusCode)
	}

	resp, err = http.Get(server.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		// Second scrape's own static baseline (2*16, 2*8) plus the one
		// completed request's live contribution (+16, +8).
		"vllm:prompt_tokens_total{model_name=\"fake-model\"} 48",
		"vllm:generation_tokens_total{model_name=\"fake-model\"} 24",
		// The request already completed synchronously by the time this
		// scrape runs, so running is back to its idle baseline.
		`vllm:num_requests_running{model_name="fake-model"} 0`,
	} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("post-chat exposition missing %q:\n%s", want, after)
		}
	}
	if len(logs) < 2 || !strings.Contains(logs[0], "Received request req-") {
		t.Fatalf("engine request logs missing: %v", logs)
	}
}

func post(t *testing.T, url, contentType string, body io.Reader) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestStreamingChatEmitsOneDeltaPerConfiguredChunk(t *testing.T) {
	for _, tc := range []struct {
		chunks, want int
	}{{0, 1}, {1, 1}, {5, 5}} {
		server := httptest.NewServer(New(Options{Model: "fake-model", StreamChunks: tc.chunks}))
		response := post(t, server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		server.Close()
		if got := strings.Count(string(body), `"delta":{"content"`); got != tc.want {
			t.Fatalf("StreamChunks=%d: %d content deltas, want %d: %q", tc.chunks, got, tc.want, body)
		}
		if !strings.HasSuffix(string(body), "data: [DONE]\n\n") || !strings.Contains(string(body), `"usage"`) {
			t.Fatalf("StreamChunks=%d: stream must still end with usage and [DONE]: %q", tc.chunks, body)
		}
	}
}

func TestStaticGaugesIgnoreLiveTraffic(t *testing.T) {
	// The same held request is visible in the live gauge (2 configured + 1
	// waiting) and invisible in the static one, which is the whole contract.
	for _, tc := range []struct {
		static bool
		want   string
	}{{false, " 3"}, {true, " 2"}} {
		server := httptest.NewServer(New(Options{Model: "fake-model", StaticGauges: tc.static, MetricsRequestsWaiting: 2, TTFTDelay: 1500 * time.Millisecond}))
		ctx, cancel := context.WithCancel(context.Background())
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"fake-model","messages":[{"role":"user","content":"hi"}]}`))
		request.Header.Set("Content-Type", "application/json")
		go func() { _, _ = http.DefaultClient.Do(request) }()
		// Sample while the request is held (the engine releases it after 1.5s).
		got := ""
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if got = waitingGauge(t, server.URL); got != " 2" {
				break
			}
		}
		cancel()
		server.Close()
		if got != tc.want {
			t.Fatalf("StaticGauges=%v: waiting gauge ends %q, want %q", tc.static, got, tc.want)
		}
	}
}

func waitingGauge(t *testing.T, base string) string {
	t.Helper()
	response, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "vllm:num_requests_waiting{") {
			return line[strings.LastIndex(line, " "):]
		}
	}
	t.Fatalf("metrics lack the waiting gauge: %s", body)
	return ""
}
