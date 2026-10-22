package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// PDOptions makes the fixture behave like a vLLM NixlConnector instance
// (artifacts/archive/2026-09-28-pd-disaggregation/evidence/wire/). The producer
// answers a request carrying kv_transfer_params.do_remote_decode with one
// token and the params the consumer needs, and keeps the "KV" until it is
// pulled or its lease expires. The consumer pulls it from the producer's side
// channel before generating; without params it computes the prefill itself.
type PDOptions struct {
	// Role is the connector role: "kv_producer" or "kv_consumer".
	Role string
	// EngineID identifies this engine in the params it hands out.
	EngineID string
	// SideHost and SidePort are where this engine's side channel listens and
	// what a producer advertises as remote_host/remote_port.
	SideHost string
	SidePort int
	// FailurePolicy is kv_load_failure_policy: "fail" (default) answers a
	// failed pull with 500, "recompute" computes locally instead.
	FailurePolicy string
	// Disabled emulates an engine whose NIXL connector was replaced (a
	// --kv-offloading-size beside the config): the producer answers without
	// params, as vLLM 0.25.1 was measured to.
	Disabled bool
	// Lease is how long a producer keeps an unpulled request.
	Lease time.Duration
	// BytesPerToken is what one transferred token counts as.
	BytesPerToken int
	// Crash is called when the consumer receives params missing a field the
	// engine indexes directly: vLLM's EngineCore dies with a KeyError there
	// (measured). Nil makes the fixture answer 500 instead.
	Crash func(reason string)
	// Concurrent selects SGLang's handoff: both instances get the request at
	// once with bootstrap_host/port/room; the producer registers the room,
	// the consumer fetches it from the producer's bootstrap server (with a
	// short retry, the two race) and refuses a request without one.
	Concurrent bool
	// Dial reaches a peer's side channel; nil dials the address as given. A
	// test cluster whose nodes all live on one host maps node addresses here.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// peerClient is the HTTP client the engine pulls KV from peers with.
func (s *pdState) peerClient(timeout time.Duration) http.Client {
	client := http.Client{Timeout: timeout}
	if s.opts.Dial != nil {
		client.Transport = &http.Transport{DialContext: s.opts.Dial}
	}
	return client
}

// PDFromLaunch reads the PD role from a launch's argv and environment the way
// vLLM does; nil when the launch carries no NIXL connector.
func PDFromLaunch(argv []string, env map[string]string) *PDOptions {
	if role := valueFlag(argv, "--disaggregation-mode"); role == "prefill" || role == "decode" {
		port, _ := strconv.Atoi(valueFlag(argv, "--disaggregation-bootstrap-port"))
		if port == 0 {
			port = 8998 // SGLang's default bootstrap port
		}
		pd := &PDOptions{Role: "kv_producer", SideHost: valueFlag(argv, "--host"), SidePort: port, Concurrent: true}
		if role == "decode" {
			pd.Role, pd.SidePort = "kv_consumer", 0
		}
		return pd
	}
	raw := valueFlag(argv, "--kv-transfer-config")
	if raw == "" {
		return nil
	}
	var config struct {
		Connector string `json:"kv_connector"`
		Role      string `json:"kv_role"`
		Policy    string `json:"kv_load_failure_policy"`
		Extra     struct {
			Connectors []struct {
				Connector string `json:"kv_connector"`
				Role      string `json:"kv_role"`
			} `json:"connectors"`
		} `json:"kv_connector_extra_config"`
	}
	if json.Unmarshal([]byte(raw), &config) != nil {
		return nil
	}
	role := ""
	switch config.Connector {
	case "NixlConnector":
		role = config.Role
	case "MultiConnector":
		for _, child := range config.Extra.Connectors {
			if child.Connector == "NixlConnector" {
				role = child.Role
			}
		}
	}
	if role != "kv_producer" && role != "kv_consumer" && role != "kv_both" {
		return nil
	}
	port, _ := strconv.Atoi(env["VLLM_NIXL_SIDE_CHANNEL_PORT"])
	if port == 0 {
		port = 5600 // vLLM's default side channel port
	}
	host := env["VLLM_NIXL_SIDE_CHANNEL_HOST"]
	if host == "" {
		host = "localhost"
	}
	return &PDOptions{
		Role: role, SideHost: host, SidePort: port, FailurePolicy: config.Policy,
		Disabled: valueFlag(argv, "--kv-offloading-size") != "",
	}
}

func valueFlag(args []string, name string) string {
	for i, arg := range args {
		if arg == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"=")
		}
	}
	return ""
}

// pdState is one engine's PD bookkeeping and counters.
type pdState struct {
	opts    PDOptions
	mu      sync.Mutex
	pending map[string]pendingKV
	seq     atomic.Uint64

	localTokens, cacheTokens, externalTokens atomic.Int64
	transferBytes, transfers, failed         atomic.Int64
	expired                                  atomic.Int64
}

type pendingKV struct {
	tokens  int
	expires time.Time
}

func newPDState(opts PDOptions) *pdState {
	if opts.Lease <= 0 {
		opts.Lease = 30 * time.Second
	}
	if opts.BytesPerToken <= 0 {
		opts.BytesPerToken = 1024
	}
	if opts.EngineID == "" {
		opts.EngineID = fmt.Sprintf("fake-engine-%d", time.Now().UnixNano())
	}
	return &pdState{opts: opts, pending: map[string]pendingKV{}}
}

// SideChannel serves the producer's KV to consumers: GET /kv/<request id>
// returns the token count once and forgets it. A request that was never made,
// already pulled or expired is 404, which a consumer reports as a failed
// transfer.
func (s *pdState) SideChannel() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/kv/")
		s.mu.Lock()
		entry, ok := s.pending[id]
		delete(s.pending, id)
		s.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if time.Now().After(entry.expires) {
			s.expired.Add(1)
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]int{"tokens": entry.tokens})
	})
}

// ServeSideChannel listens on the side channel port on every address, as the
// engine binds its configured host, until the listener fails.
func (s *pdState) ServeSideChannel() (func(), error) {
	if s.opts.SidePort == 0 {
		return func() {}, nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", s.opts.SidePort))
	if err != nil {
		return nil, err
	}
	server := &http.Server{Handler: s.SideChannel()}
	go func() { _ = server.Serve(ln) }()
	return func() { _ = server.Close() }, nil
}

// pdPromptTokens is the prompt size the fixture reports: the configured
// count, so usage and the PD counters agree.
func pdPromptTokens(opts Options) int {
	if opts.PromptTokens > 0 {
		return opts.PromptTokens
	}
	return 16
}

// wrap intercepts the two-stage paths. Everything else passes through.
func (s *pdState) wrap(opts Options, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/completions") {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Params map[string]any `json:"kv_transfer_params"`
			Room   *int64         `json:"bootstrap_room"`
			Host   string         `json:"bootstrap_host"`
			Port   int            `json:"bootstrap_port"`
		}
		_ = json.Unmarshal(body, &request)
		tokens := pdPromptTokens(opts)
		if s.opts.Concurrent {
			if !s.concurrent(w, request.Room, request.Host, request.Port, tokens) {
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		switch {
		case s.opts.Role != "kv_consumer" && request.Params != nil && request.Params["do_remote_decode"] == true:
			s.localTokens.Add(int64(tokens))
			s.producerAnswer(w, r.URL.Path, tokens)
			return
		case s.opts.Role != "kv_producer" && request.Params != nil && request.Params["do_remote_prefill"] == true:
			if !s.consume(w, request.Params, tokens) {
				return
			}
		default:
			s.localTokens.Add(int64(tokens))
		}
		next.ServeHTTP(w, r)
	})
}

func (s *pdState) producerAnswer(w http.ResponseWriter, path string, tokens int) {
	id := fmt.Sprintf("chatcmpl-%016x-%08x", time.Now().UnixNano(), s.seq.Add(1))
	var params any
	if !s.opts.Disabled {
		s.mu.Lock()
		s.pending[id] = pendingKV{tokens: tokens, expires: time.Now().Add(s.opts.Lease)}
		s.mu.Unlock()
		blocks := make([]int, (tokens+15)/16)
		for i := range blocks {
			blocks[i] = i + 1
		}
		params = map[string]any{
			"do_remote_prefill": true, "do_remote_decode": false,
			"remote_block_ids": [][]int{blocks}, "remote_engine_id": s.opts.EngineID,
			"remote_request_id": id, "remote_host": s.opts.SideHost, "remote_port": s.opts.SidePort,
			"tp_size": 1, "remote_num_tokens": tokens,
		}
	}
	usage := map[string]any{"prompt_tokens": tokens, "completion_tokens": 1, "total_tokens": tokens + 1, "prompt_tokens_details": nil}
	answer := map[string]any{"id": id, "created": time.Now().Unix(), "model": "fake", "usage": usage, "kv_transfer_params": params}
	if path == "/v1/completions" {
		answer["object"] = "text_completion"
		answer["choices"] = []any{map[string]any{"index": 0, "text": " The", "finish_reason": "length"}}
	} else {
		answer["object"] = "chat.completion"
		answer["choices"] = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "The"}, "finish_reason": "length"}}
	}
	writeJSON(w, answer)
}

// consume pulls the KV the params point at. It reports whether the request
// may go on to generation.
func (s *pdState) consume(w http.ResponseWriter, params map[string]any, tokens int) bool {
	for _, key := range []string{"remote_request_id", "remote_engine_id", "remote_host", "remote_port", "remote_block_ids"} {
		if _, ok := params[key]; !ok {
			reason := "KeyError: '" + key + "'"
			if s.opts.Crash != nil {
				s.opts.Crash(reason)
			}
			writeAPIError(w, http.StatusInternalServerError, "InternalServerError", "EngineCore encountered an issue: "+reason)
			return false
		}
	}
	host := fmt.Sprint(params["remote_host"])
	port := fmt.Sprint(params["remote_port"])
	if f, ok := params["remote_port"].(float64); ok {
		port = strconv.Itoa(int(f))
	}
	client := s.peerClient(5 * time.Second)
	resp, err := client.Get(fmt.Sprintf("http://%s/kv/%v", net.JoinHostPort(host, port), params["remote_request_id"]))
	if err == nil && resp.StatusCode == http.StatusOK {
		var pulled struct {
			Tokens int `json:"tokens"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&pulled)
		_ = resp.Body.Close()
		s.externalTokens.Add(int64(pulled.Tokens))
		s.transferBytes.Add(int64(pulled.Tokens * s.opts.BytesPerToken))
		s.transfers.Add(1)
		return true
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	s.failed.Add(1)
	if s.opts.FailurePolicy == "recompute" {
		s.localTokens.Add(int64(tokens))
		return true
	}
	writeAPIError(w, http.StatusInternalServerError, "InternalServerError", "Internal server error")
	return false
}

// metrics is the NIXL and prompt-source exposition vLLM adds for PD.
func (s *pdState) metrics(modelName string) string {
	var b strings.Builder
	fmt.Fprintln(&b, "# TYPE vllm:prompt_tokens_by_source_total counter")
	for _, source := range []struct {
		name  string
		value int64
	}{{"local_compute", s.localTokens.Load()}, {"local_cache_hit", s.cacheTokens.Load()}, {"external_kv_transfer", s.externalTokens.Load()}} {
		fmt.Fprintf(&b, "vllm:prompt_tokens_by_source_total{engine=\"0\",model_name=%q,source=%q} %d\n", modelName, source.name, source.value)
	}
	fmt.Fprintln(&b, "# TYPE vllm:nixl_bytes_transferred histogram")
	fmt.Fprintf(&b, "vllm:nixl_bytes_transferred_sum{engine=\"0\",model_name=%q} %d\n", modelName, s.transferBytes.Load())
	fmt.Fprintf(&b, "vllm:nixl_bytes_transferred_count{engine=\"0\",model_name=%q} %d\n", modelName, s.transfers.Load())
	fmt.Fprintln(&b, "# TYPE vllm:nixl_num_failed_transfers_total counter")
	fmt.Fprintf(&b, "vllm:nixl_num_failed_transfers_total{engine=\"0\",model_name=%q} %d\n", modelName, s.failed.Load())
	fmt.Fprintln(&b, "# TYPE vllm:nixl_num_kv_expired_reqs_total counter")
	fmt.Fprintf(&b, "vllm:nixl_num_kv_expired_reqs_total{engine=\"0\",model_name=%q} %d\n", modelName, s.expired.Load())
	return b.String()
}

// concurrent handles one SGLang-style request. It reports whether the
// request may go on to generation.
func (s *pdState) concurrent(w http.ResponseWriter, room *int64, host string, port int, tokens int) bool {
	if s.opts.Role == "kv_producer" {
		s.localTokens.Add(int64(tokens))
		if room != nil {
			s.mu.Lock()
			s.pending[fmt.Sprintf("room-%d", *room)] = pendingKV{tokens: tokens, expires: time.Now().Add(s.opts.Lease)}
			s.mu.Unlock()
		}
		return true
	}
	if room == nil || host == "" || port == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request_error", "a decode server in disaggregation mode needs bootstrap_host, bootstrap_port and bootstrap_room")
		return false
	}
	client := s.peerClient(time.Second)
	url := fmt.Sprintf("http://%s/kv/room-%d", net.JoinHostPort(host, strconv.Itoa(port)), *room)
	for attempt := 0; attempt < 30; attempt++ {
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			var pulled struct {
				Tokens int `json:"tokens"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&pulled)
			_ = resp.Body.Close()
			s.externalTokens.Add(int64(pulled.Tokens))
			s.transferBytes.Add(int64(pulled.Tokens * s.opts.BytesPerToken))
			s.transfers.Add(1)
			return true
		}
		if resp != nil {
			_ = resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.failed.Add(1)
	writeAPIError(w, http.StatusInternalServerError, "InternalServerError", "KV transfer from the prefill server failed")
	return false
}
