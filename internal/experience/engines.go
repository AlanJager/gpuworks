package experience

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/engines"
)

// loadSlowdown lengthens a simulated engine's pacing (pace) for each other
// request in flight, so latency rises under concurrency as on a real engine.
const loadSlowdown = 0.02

// engineBatch is how many requests an engine runs at once; more wait.
const engineBatch = 24

// engineHost answers for every simulated instance's engine. A simulated
// node's runtime claims an engine port for a model when an instance starts
// (POST /claim) and releases it when the instance stops (POST /release); a
// node agent that starts afresh releases all its node held (POST /release
// without a container), so a restarted node leaves no engine behind.
// Every node runs on this one host, so each node takes its engine ports
// from a slice of the range of its own (simulate.Spec.PortSlot); a port is
// one node's.
type engineHost struct {
	token string
	log   *slog.Logger

	mu     sync.Mutex
	closed bool // once serve has ended: no claim opens a listener after
	ports  map[int]*enginePort
	owner  map[string]int // container ("<node>/<id>") -> port
}

func newEngineHost(token string, log *slog.Logger) *engineHost {
	return &engineHost{token: token, log: log, ports: map[int]*enginePort{}, owner: map[string]int{}}
}

// serve answers the nodes' claims on ln until ctx ends, then closes every
// engine listener.
func (h *engineHost) serve(ctx context.Context, ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /claim", h.claim)
	mux.HandleFunc("POST /release", h.release)
	srv := &http.Server{Handler: h.authorized(mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
		h.mu.Lock()
		defer h.mu.Unlock()
		h.closed = true
		for _, p := range h.ports {
			_ = p.srv.Close()
		}
	}()
	_ = srv.Serve(ln)
}

func (h *engineHost) authorized(next http.Handler) http.Handler {
	want := []byte("Bearer " + h.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type claimRequest struct {
	Node      string `json:"node"`
	Container string `json:"container"`
	Port      int    `json:"port"`
	Model     string `json:"model"`
}

func (h *engineHost) claim(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Container == "" || req.Model == "" {
		http.Error(w, "container and model are required", http.StatusBadRequest)
		return
	}
	if req.Port < 1024 || req.Port > 65535 {
		http.Error(w, "port out of range", http.StatusBadRequest)
		return
	}
	key := req.Node + "/" + req.Container
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		http.Error(w, "the experience environment is stopping", http.StatusServiceUnavailable)
		return
	}
	p := h.ports[req.Port]
	if p != nil && p.node != req.Node {
		// Each port answers for one node's instance: the agent reads its
		// engine's metrics there, which must be that instance's own.
		http.Error(w, fmt.Sprintf("engine port %d is held by another node", req.Port), http.StatusConflict)
		return
	}
	if p == nil {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(req.Port)))
		if err != nil {
			http.Error(w, fmt.Sprintf("engine port %d: %v", req.Port, err), http.StatusConflict)
			return
		}
		p = newEnginePort()
		p.node = req.Node
		p.srv = &http.Server{Handler: p.handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = p.srv.Serve(ln) }()
		h.ports[req.Port] = p
	}
	p.add(key, req.Model)
	h.owner[key] = req.Port
	w.WriteHeader(http.StatusNoContent)
}

func (h *engineHost) release(w http.ResponseWriter, r *http.Request) {
	var req claimRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Node == "" {
		http.Error(w, "node is required", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if req.Container != "" {
		h.releaseLocked(req.Node + "/" + req.Container)
	} else {
		for key := range h.owner {
			if strings.HasPrefix(key, req.Node+"/") {
				h.releaseLocked(key)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// releaseLocked drops one claim, closing its port once nothing holds it.
// h.mu is held.
func (h *engineHost) releaseLocked(key string) {
	port, ok := h.owner[key]
	if !ok {
		return
	}
	delete(h.owner, key)
	if p := h.ports[port]; p != nil && p.remove(key) {
		_ = p.srv.Close()
		delete(h.ports, port)
	}
}

// enginePort is one engine listener and the models claimed on it.
type enginePort struct {
	srv  *http.Server
	node string // the node whose instance holds the port

	mu     sync.Mutex
	claims map[string]string // container -> model

	running atomic.Int64
	stats   *engineStats
}

func newEnginePort() *enginePort {
	return &enginePort{claims: map[string]string{}, stats: newEngineStats()}
}

func (p *enginePort) add(container, model string) {
	p.mu.Lock()
	p.claims[container] = model
	p.mu.Unlock()
}

// remove drops a claim and reports whether the port is now unclaimed.
func (p *enginePort) remove(container string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.claims, container)
	return len(p.claims) == 0
}

func (p *enginePort) models() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, m := range p.claims {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Strings(out)
	return out
}

func (p *enginePort) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		data := []map[string]any{}
		for _, m := range p.models() {
			data = append(data, map[string]any{"id": m, "object": "model", "owned_by": engines.VLLM})
		}
		writeJSON(w, map[string]any{"object": "list", "data": data})
	})
	mux.HandleFunc("POST /v1/chat/completions", p.chat)
	mux.HandleFunc("GET /metrics", p.metrics)
	return mux
}

type chatRequest struct {
	Model     string          `json:"model"`
	Stream    bool            `json:"stream"`
	Messages  json.RawMessage `json:"messages"`
	MaxTokens int             `json:"max_tokens"`
	IgnoreEOS bool            `json:"ignore_eos"`
}

// chat answers with the canned reply (scenario.go), streamed one rune per
// token when asked, with usage counting the prompt's words and a share of
// them as served from the prefix cache.
func (p *enginePort) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil || len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages is required")
		return
	}
	if req.Model == "" {
		if models := p.models(); len(models) > 0 {
			req.Model = models[0]
		}
	}
	start := time.Now()
	ttft, tpot := pace(req.Model)
	running := p.running.Add(1)
	defer p.running.Add(-1)
	// Past its batch the engine queues: those requests wait their turn.
	queued := time.Duration(max(0, running-engineBatch)) * 20 * time.Millisecond
	factor := 1 + loadSlowdown*float64(running-1)
	// Latencies spread the way engines' do: most near the typical value, a
	// tail well above it.
	firstDelay := time.Duration(float64(ttft)*factor*math.Exp(rand.NormFloat64()*0.35)) + queued
	tokenDelay := time.Duration(float64(tpot) * factor * math.Exp(rand.NormFloat64()*0.15))
	if !sleep(r.Context(), firstDelay) {
		return
	}

	tokens := replyTokens(reply(req.Model, req.Messages), req.MaxTokens, req.IgnoreEOS)
	prompt := promptWords(req.Messages)
	cached := int(float64(prompt) * (0.35 + rand.Float64()*0.45))
	usage := map[string]any{
		"prompt_tokens": prompt, "completion_tokens": len(tokens), "total_tokens": prompt + len(tokens),
		"prompt_tokens_details": map[string]int{"cached_tokens": cached},
	}
	id := fmt.Sprintf("chatcmpl-%x", rand.Uint64())
	if req.Stream {
		if !p.stream(r.Context(), w, id, req.Model, tokens, tokenDelay, usage) {
			return
		}
	} else {
		if !sleep(r.Context(), tokenDelay*time.Duration(len(tokens))) {
			return
		}
		writeJSON(w, map[string]any{"id": id, "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": string(tokens)}, "finish_reason": "stop"}},
			"usage":   usage})
	}
	p.stats.observe(request{
		prompt: prompt, cached: cached, generated: len(tokens),
		queue: queued, ttft: firstDelay, tpot: tokenDelay, e2e: time.Since(start),
	})
}

// pace is how fast model answers at rest: its first token, then each next
// one. A starter answers at the latency its history records (starters);
// a deployment a visitor made is paced by its size, a larger model slower.
func pace(model string) (ttft, tpot time.Duration) {
	for _, s := range starters {
		if s.Name == model {
			return time.Duration(s.TTFT * float64(time.Millisecond)), time.Duration(s.TPOT * float64(time.Millisecond))
		}
	}
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "32b"), strings.Contains(m, "27b"):
		return 420 * time.Millisecond, 24 * time.Millisecond
	case strings.Contains(m, "14b"):
		return 320 * time.Millisecond, 18 * time.Millisecond
	}
	return 240 * time.Millisecond, 13 * time.Millisecond
}

// stream writes tokens one per delta, then the usage chunk and [DONE], as an
// OpenAI-compatible engine does. It reports whether the client stayed.
func (p *enginePort) stream(ctx context.Context, w http.ResponseWriter, id, model string, tokens []rune, delay time.Duration, usage map[string]any) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	created := time.Now().Unix()
	send := func(payload map[string]any) bool {
		payload["id"], payload["object"], payload["created"], payload["model"] = id, "chat.completion.chunk", created, model
		raw, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	// Tokens go out a few to a chunk, as an engine flushes under load; the
	// pacing per token stays.
	const perChunk = 4
	for i := 0; i < len(tokens); i += perChunk {
		chunk := tokens[i:min(i+perChunk, len(tokens))]
		if i > 0 && !sleep(ctx, delay*time.Duration(len(chunk))) {
			return false
		}
		if !send(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": string(chunk)}}}}) {
			return false
		}
	}
	send(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}}})
	send(map[string]any{"choices": []any{}, "usage": usage})
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err == nil
}

// replyTokens is what a reply streams, one rune per token: cut at max_tokens,
// and repeated up to it when the client asks the engine to ignore
// end-of-sequence (as an evaluation does to fix the output length).
func replyTokens(answer string, maxTokens int, ignoreEOS bool) []rune {
	runes := []rune(answer)
	if len(runes) == 0 || maxTokens <= 0 || (!ignoreEOS && maxTokens >= len(runes)) {
		return runes
	}
	if !ignoreEOS || maxTokens <= len(runes) {
		return runes[:maxTokens]
	}
	out := make([]rune, maxTokens)
	for i := range out {
		out[i] = runes[i%len(runes)]
	}
	return out
}

// promptWords counts the words of the messages' text: the prompt tokens the
// engine reports.
func promptWords(raw json.RawMessage) int {
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(raw, &messages)
	words := 0
	for _, m := range messages {
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			words += len(strings.Fields(text))
		}
	}
	return max(words, 1)
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request_error", "code": status}})
}
