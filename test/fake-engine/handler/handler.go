// Package handler is the HTTP contract shared by the standalone fake-engine
// process and the L1 in-process FakeDriver.
package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Options struct {
	Voices []string
	Model  string
	// Models, when set, is what /v1/models lists instead of Model alone: a
	// handler serving many instances at once (a shared test port) answers
	// for each of their models, as each instance's own engine would.
	Models           func() []string
	Health503        bool
	ResponseText     string
	TTFTDelay        time.Duration
	TPOTDelay        time.Duration
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
	// StreamChunks is how many content deltas a streaming chat completion
	// emits, TPOTDelay apart, the way an engine streams one delta per token.
	// Zero or one keeps the single-delta response.
	StreamChunks int
	// StaticGauges makes /metrics report only the configured running and
	// waiting gauges, without the live in-flight counts. A handler that
	// serves many instances at once (a shared test port) would otherwise
	// report the sum of all of them as each one's queue.
	StaticGauges bool
	// Logf receives engine-style INFO lines (startup banner, received request
	// messages). Real engines log to stdout and the product ships those lines
	// to the log store, so the fixture must produce them too. Nil disables.
	Logf func(format string, args ...any)

	// Dialect selects which family of Prometheus metric names /metrics
	// exposes: "vllm" (the default when empty) or "sglang". It mirrors the
	// --dialect argv main.go already validates.
	Dialect string
	// MetricsObservationsPerScrape is how many identical-value observations
	// each histogram family (and how many PromptTokens/CompletionTokens worth
	// of new tokens each counter) gains on every /metrics scrape (default 1
	// when <= 0). Every counter/histogram is therefore stateful across
	// scrapes — two successive scrapes show nonzero rate() — while gauges
	// (MetricsRequestsRunning/Waiting/KVCacheUsagePercent) stay fixed.
	MetricsObservationsPerScrape int
	// MetricsTTFTSeconds..MetricsDecodeSeconds are the single fixed value
	// every observation this run takes for that family: real engines don't
	// expose a distribution shape at the fixture level, so every observation
	// landing in the same value's bucket is the simplest faithful choice.
	MetricsTTFTSeconds    float64
	MetricsTPOTSeconds    float64
	MetricsE2ESeconds     float64
	MetricsQueueSeconds   float64
	MetricsPrefillSeconds float64
	MetricsDecodeSeconds  float64
	// MetricsPreemptionsPerScrape and MetricsRequestSuccess{Stop,Length,
	// Abort}PerScrape are counter increments applied on every scrape (real
	// engines' counters only ever grow, they are never a fixed total).
	// vllm:request_success_total is always split by finished_reason in real
	// vLLM — there is no bare/unlabeled total to configure.
	MetricsPreemptionsPerScrape          int
	MetricsRequestSuccessStopPerScrape   int
	MetricsRequestSuccessLengthPerScrape int
	MetricsRequestSuccessAbortPerScrape  int
	// MetricsRequestsRunning/MetricsRequestsWaiting/MetricsKVCacheUsagePercent
	// are gauges: fixed at this value on every scrape.
	MetricsRequestsRunning     int
	MetricsRequestsWaiting     int
	MetricsKVCacheUsagePercent float64

	// StatusOverride, when nonzero, makes /v1/chat/completions immediately
	// respond with this HTTP status (a JSON error body) instead of serving
	// the request - a deterministic way for a test to make the proxy see a
	// 429/5xx from a real upstream instance without actually killing it (see
	// FAKE_ENGINE_STATUS_OVERRIDE and the /debug/status-override endpoint,
	// which lets a running instance's override be changed at runtime the
	// same way /sleep toggles sleep state).
	StatusOverride int
	// WakeFailures and WakeDelay make wake behavior fail or pause deterministically
	// for sleep/wake integration tests.
	WakeFailures int
	WakeDelay    time.Duration
	// PartialWakeFailures applies the weight allocation before returning an
	// error, matching an L2 wake that fails during reload.
	PartialWakeFailures int
	// SleepDelay delays applying the sleep transition, including after an HTTP
	// client timeout, to model wait-mode requests that finish in the engine.
	SleepDelay time.Duration
	// PD makes the engine a PD disaggregation producer or consumer (pd.go).
	PD *PDOptions
	// ExtraMetrics appends exposition lines to /metrics.
	ExtraMetrics func() string
}

// Engine is the fixture's HTTP surface, plus the PD side channel when the
// engine plays a PD role.
type Engine struct {
	http.Handler
	pd *pdState
}

// ServeSideChannel starts the PD side channel listener; a no-op without a PD
// role. The returned function stops it.
func (e *Engine) ServeSideChannel() (func(), error) {
	if e == nil || e.pd == nil {
		return func() {}, nil
	}
	return e.pd.ServeSideChannel()
}

// engineStats tracks live in-flight request state so /metrics gauges/counters
// move with real traffic: registerMetrics adds these on top of the
// per-scrape deterministic values above (running/waiting/tokens), so a test
// or harness that never drives real requests still sees the exact configured
// numbers, while one that does sees them shift accordingly.
type engineStats struct {
	running          atomic.Int64
	waiting          atomic.Int64
	promptTokens     atomic.Int64
	generationTokens atomic.Int64
	requests         atomic.Uint64
	// statusOverride is the live, mutable form of Options.StatusOverride: it
	// starts at the option's value but can be changed at runtime through
	// /debug/status-override, the same way /sleep mutates `sleeping` below -
	// this lets one already-running instance be flipped between "answer
	// normally" and "force this HTTP status" without a redeploy.
	statusOverride atomic.Int32
}

type videoJob struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	PollsRemaining int    `json:"-"`
}

func New(opts Options) *Engine {
	if opts.ResponseText == "" {
		opts.ResponseText = "hello from fake-engine"
	}
	var pd *pdState
	if opts.PD != nil {
		pd = newPDState(*opts.PD)
		modelName := opts.Model
		previous := opts.ExtraMetrics
		opts.ExtraMetrics = func() string {
			extra := pd.metrics(modelName)
			if previous != nil {
				extra += previous()
			}
			return extra
		}
	}
	sleep := newSleepState()
	sleep.wakeFailures = opts.WakeFailures
	sleep.wakeDelay = opts.WakeDelay
	sleep.partialWakeFailures = opts.PartialWakeFailures
	sleep.sleepDelay = opts.SleepDelay
	var nextVideoID atomic.Uint64
	var videosMu sync.Mutex
	videos := map[string]*videoJob{}
	var stats engineStats
	stats.statusOverride.Store(int32(opts.StatusOverride))
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	mux := http.NewServeMux()
	registerDiscovery(mux, opts)
	registerMetrics(mux, opts, &stats)
	registerTokenize(mux, opts)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if opts.Health503 {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		ids := []string{opts.Model}
		if opts.Models != nil {
			ids = opts.Models()
		}
		data := make([]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id})
		}
		writeJSON(w, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if override := stats.statusOverride.Load(); override != 0 {
			logf("Forcing status override %d for a chat completion request", override)
			writeAPIError(w, int(override), "server_error", "fake-engine: forced status override")
			return
		}
		var request struct {
			Stream   bool            `json:"stream"`
			Messages json.RawMessage `json:"messages"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if len(request.Messages) == 0 || string(request.Messages) == "null" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "messages is required")
			return
		}
		requestID := fmt.Sprintf("req-%08x", stats.requests.Add(1))
		logf("Received request %s: prompt=%d tok, completion=%d tok", requestID, opts.PromptTokens, opts.CompletionTokens)
		stats.waiting.Add(1)
		if opts.TTFTDelay > 0 {
			time.Sleep(opts.TTFTDelay)
		}
		stats.waiting.Add(-1)
		stats.running.Add(1)
		defer stats.running.Add(-1)
		stats.promptTokens.Add(int64(opts.PromptTokens))
		stats.generationTokens.Add(int64(opts.CompletionTokens))
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			flusher, _ := w.(http.Flusher)
			encoded, encodeErr := json.Marshal(opts.ResponseText)
			if encodeErr != nil {
				writeAPIError(w, http.StatusInternalServerError, "invalid_request_error", encodeErr.Error())
				return
			}
			chunk := []byte("data: {\"id\":\"fake-engine-completion\",\"choices\":[{\"delta\":{\"content\":" + string(encoded) + "}}]}\n\n")
			for i := 0; i < max(opts.StreamChunks, 1); i++ {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				if opts.TPOTDelay > 0 {
					time.Sleep(opts.TPOTDelay)
				}
			}
			usageBytes, _ := json.Marshal(map[string]any{"choices": []any{}, "usage": usage(opts)})
			_, _ = w.Write(append(append([]byte("data: "), usageBytes...), []byte("\n\n")...))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		writeJSON(w, map[string]any{"id": "fake-engine-completion", "object": "chat.completion", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": opts.ResponseText}, "finish_reason": "stop"}}, "usage": usage(opts)})
		logf("Generated response for %s in one pass", requestID)
	})
	mux.HandleFunc("/v1/completions", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if len(request.Prompt) == 0 || string(request.Prompt) == "null" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
			return
		}
		stats.requests.Add(1)
		stats.promptTokens.Add(int64(opts.PromptTokens))
		stats.generationTokens.Add(int64(opts.CompletionTokens))
		writeJSON(w, map[string]any{"id": "fake-engine-completion", "object": "text_completion", "choices": []any{map[string]any{"index": 0, "text": opts.ResponseText, "finish_reason": "stop"}}, "usage": usage(opts)})
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Input any `json:"input"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if request.Input == nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
			return
		}
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"object": "embedding", "index": 0, "embedding": []float64{0.125, -0.25, 0.5}}}, "model": opts.Model, "usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1}})
	})
	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Query     string            `json:"query"`
			Documents []json.RawMessage `json:"documents"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if strings.TrimSpace(request.Query) == "" || len(request.Documents) == 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "query and documents are required")
			return
		}
		results := make([]map[string]any, len(request.Documents))
		for index := range request.Documents {
			results[index] = map[string]any{"index": index, "relevance_score": 1.0 / float64(index+1)}
		}
		writeJSON(w, map[string]any{"id": "fake-engine-rerank", "model": opts.Model, "results": results})
	})
	mux.HandleFunc("/v1/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "multipart/form-data is required")
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil || strings.TrimSpace(r.FormValue("model")) == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "model and file are required")
			return
		}
		defer file.Close()
		if _, err := io.Copy(io.Discard, file); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read uploaded audio")
			return
		}
		writeJSON(w, map[string]any{"text": opts.ResponseText})
	})
	mux.HandleFunc("/v1/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Input string `json:"input"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if strings.TrimSpace(request.Input) == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "input is required")
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(fakeWAV)
	})
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Prompt string `json:"prompt"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if strings.TrimSpace(request.Prompt) == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
			return
		}
		writeJSON(w, map[string]any{"created": 1, "data": []any{map[string]string{"b64_json": fakePNGBase64}}})
	})
	mux.HandleFunc("/v1/videos", func(w http.ResponseWriter, r *http.Request) {
		if !requireMethod(w, r, http.MethodPost) {
			return
		}
		var request struct {
			Prompt  string `json:"prompt"`
			Seconds int    `json:"seconds"`
		}
		if err := decodeJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if strings.TrimSpace(request.Prompt) == "" {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "prompt is required")
			return
		}
		id := fmt.Sprintf("video-%d", nextVideoID.Add(1))
		videosMu.Lock()
		pollsRemaining := 0
		if request.Seconds >= 60 {
			pollsRemaining = 3
		}
		status := "queued"
		if strings.Contains(strings.ToLower(request.Prompt), "fail requested") {
			status = "failed"
		}
		videos[id] = &videoJob{ID: id, Status: status, PollsRemaining: pollsRemaining}
		videosMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": status})
	})
	mux.HandleFunc("/v1/videos/", func(w http.ResponseWriter, r *http.Request) {
		id, content, ok := parseVideoPath(r.URL.Path)
		if !ok {
			writeAPIError(w, http.StatusNotFound, "not_found_error", "video task was not found")
			return
		}
		videosMu.Lock()
		job := videos[id]
		if job == nil {
			videosMu.Unlock()
			writeAPIError(w, http.StatusNotFound, "not_found_error", "video task was not found")
			return
		}
		switch {
		case content && r.Method == http.MethodGet:
			if job.Status == "queued" {
				job.Status = "completed"
			}
			status := job.Status
			videosMu.Unlock()
			if status != "completed" {
				writeAPIError(w, http.StatusConflict, "invalid_request_error", "video content is not available")
				return
			}
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write(fakeMP4)
		case !content && r.Method == http.MethodGet:
			if job.Status == "queued" && job.PollsRemaining > 0 {
				job.PollsRemaining--
			} else if job.Status == "queued" {
				job.Status = "completed"
			}
			response := *job
			videosMu.Unlock()
			writeJSON(w, response)
		case !content && r.Method == http.MethodDelete:
			job.Status = "deleted"
			response := *job
			videosMu.Unlock()
			writeJSON(w, response)
		default:
			videosMu.Unlock()
			w.Header().Set("Allow", allowedVideoMethods(content))
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method is not allowed")
		}
	})
	// /debug/status-override lets a test flip an already-running instance's
	// forced /v1/chat/completions status without a redeploy - the same
	// runtime-toggle shape /sleep already gives sleep state. POST ?code=N
	// sets it (N=0 clears the override, back to answering normally); GET
	// reports the current value.
	mux.HandleFunc("/debug/status-override", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			code, err := strconv.Atoi(r.URL.Query().Get("code"))
			if err != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "code must be an integer HTTP status (0 clears the override)")
				return
			}
			stats.statusOverride.Store(int32(code))
			writeJSON(w, map[string]int{"status_override": code})
		case http.MethodGet:
			writeJSON(w, map[string]int{"status_override": int(stats.statusOverride.Load())})
		default:
			w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method is not allowed")
		}
	})
	registerSleep(mux, sleep)
	var inference http.Handler = mux
	if pd != nil {
		inference = pd.wrap(opts, mux)
	}
	return &Engine{Handler: gateInference(inference, sleep), pd: pd}
}

func usage(opts Options) map[string]any {
	result := map[string]any{"prompt_tokens": opts.PromptTokens, "completion_tokens": opts.CompletionTokens, "total_tokens": opts.PromptTokens + opts.CompletionTokens}
	if opts.CachedTokens > 0 {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": opts.CachedTokens}
	}
	return result
}

func decodeJSON(r *http.Request, target any) error {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		return fmt.Errorf("application/json is required")
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method is not allowed")
	return false
}

func parseVideoPath(requestPath string) (id string, content bool, ok bool) {
	remainder := strings.TrimPrefix(requestPath, "/v1/videos/")
	parts := strings.Split(remainder, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], false, true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "content" {
		return parts[0], true, true
	}
	return "", false, false
}

func allowedVideoMethods(content bool) string {
	if content {
		return http.MethodGet
	}
	return http.MethodGet + ", " + http.MethodDelete
}

func writeAPIError(w http.ResponseWriter, status int, errorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "type": errorType, "param": nil, "code": nil}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var fakeWAV = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00\x40\x1f\x00\x00\x40\x1f\x00\x00\x01\x00\x08\x00data\x00\x00\x00\x00")

var fakeMP4 = []byte("\x00\x00\x00\x18ftypisom\x00\x00\x02\x00isomiso2")

var fakePNGBase64 = base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
