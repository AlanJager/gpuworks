package experience

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// startEngineHost serves an engine host on a loopback port and returns its
// base URL.
func startEngineHost(t *testing.T, token string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go newEngineHost(token, slog.New(slog.NewTextHandler(io.Discard, nil))).serve(ctx, ln)
	return "http://" + ln.Addr().String()
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func post(t *testing.T, url, token, body string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestEngineHostClaimRelease(t *testing.T) {
	base := startEngineHost(t, "tok")
	port := freePort(t)
	claim := `{"node":"n1","container":"sim-1","port":` + strconv.Itoa(port) + `,"model":"qwen3-8b-chat"}`
	if code := post(t, base+"/claim", "", claim); code != http.StatusUnauthorized {
		t.Fatalf("claim without token: %d", code)
	}
	if code := post(t, base+"/claim", "wrong", claim); code != http.StatusUnauthorized {
		t.Fatalf("claim with a wrong token: %d", code)
	}
	if code := post(t, base+"/claim", "tok", `{"node":"n1","container":"sim-1","port":80,"model":"m"}`); code != http.StatusBadRequest {
		t.Fatalf("claim of a privileged port: %d", code)
	}
	if code := post(t, base+"/claim", "tok", `{"node":"n1","port":30000}`); code != http.StatusBadRequest {
		t.Fatalf("claim without container: %d", code)
	}
	if code := post(t, base+"/claim", "tok", claim); code != http.StatusNoContent {
		t.Fatalf("claim: %d", code)
	}
	engine := "http://127.0.0.1:" + strconv.Itoa(port)
	resp, err := http.Get(engine + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&models)
	resp.Body.Close()
	if len(models.Data) != 1 || models.Data[0].ID != "qwen3-8b-chat" {
		t.Fatalf("models: %+v", models)
	}
	other := `{"node":"n2","container":"sim-1","port":` + strconv.Itoa(port) + `,"model":"x"}`
	if code := post(t, base+"/claim", "tok", other); code != http.StatusConflict {
		t.Fatalf("another node's claim of the port: %d", code)
	}
	if code := post(t, base+"/release", "tok", `{"node":"n9","container":"sim-9"}`); code != http.StatusNoContent {
		t.Fatalf("release of an unknown container: %d", code)
	}
	if code := post(t, base+"/release", "tok", `{"node":"n1","container":"sim-1"}`); code != http.StatusNoContent {
		t.Fatalf("release: %d", code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 200*time.Millisecond)
		if err != nil {
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the released port still answers")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if code := post(t, base+"/claim", "tok", other); code != http.StatusNoContent {
		t.Fatalf("claim after release by another node: %d", code)
	}
}

func claimedEngine(t *testing.T, model string) string {
	t.Helper()
	base := startEngineHost(t, "tok")
	port := freePort(t)
	if code := post(t, base+"/claim", "tok", `{"node":"n1","container":"c","port":`+strconv.Itoa(port)+`,"model":"`+model+`"}`); code != http.StatusNoContent {
		t.Fatalf("claim: %d", code)
	}
	return "http://127.0.0.1:" + strconv.Itoa(port)
}

func TestEngineChat(t *testing.T) {
	engine := claimedEngine(t, "Qwen/Qwen3-8B")
	resp, err := http.Post(engine+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"Qwen/Qwen3-8B","max_tokens":5,"messages":[{"role":"user","content":"one two three"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Details    struct {
				Cached int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if len(out.Choices) != 1 || len([]rune(out.Choices[0].Message.Content)) != 5 || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("reply: %+v", out)
	}
	if out.Usage.Prompt != 3 || out.Usage.Completion != 5 || out.Usage.Details.Cached > out.Usage.Prompt {
		t.Fatalf("usage: %+v", out.Usage)
	}

	resp, err = http.Post(engine+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("chat without messages: %d", resp.StatusCode)
	}
}

func TestEngineStream(t *testing.T) {
	engine := claimedEngine(t, "qwen3-8b-chat")
	resp, err := http.Post(engine+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"qwen3-8b-chat","stream":true,"max_tokens":10,"ignore_eos":true,"messages":[{"role":"user","content":"hi there"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var text strings.Builder
	var usage map[string]any
	finished, done := false, false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if line == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta        map[string]string `json:"delta"`
				FinishReason string            `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("chunk %q: %v", line, err)
		}
		if chunk.Object != "chat.completion.chunk" {
			t.Fatalf("object %q", chunk.Object)
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta["content"])
			finished = finished || c.FinishReason == "stop"
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if !done || !finished || len([]rune(text.String())) != 10 {
		t.Fatalf("done=%v finished=%v text=%q", done, finished, text.String())
	}
	if usage["completion_tokens"] != float64(10) || usage["prompt_tokens"] != float64(2) {
		t.Fatalf("usage %v", usage)
	}
	if _, ok := usage["prompt_tokens_details"].(map[string]any)["cached_tokens"]; !ok {
		t.Fatalf("usage without cached tokens: %v", usage)
	}
}

func TestEngineMetrics(t *testing.T) {
	engine := claimedEngine(t, "qwen3-32b")
	resp, err := http.Get(engine + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	page := string(raw)
	for _, family := range []string{
		"vllm:num_requests_running", "vllm:kv_cache_usage_perc", "vllm:prompt_tokens_total", "vllm:prefix_cache_hits_total",
		"vllm:request_success_total", "sglang:num_running_reqs", "sglang:token_usage", "sglang:cache_hit_rate",
	} {
		if !strings.Contains(page, family+"{") {
			t.Errorf("no %s", family)
		}
	}
	for _, histogram := range []string{"vllm:time_to_first_token_seconds", "vllm:inter_token_latency_seconds", "sglang:e2e_request_latency_seconds"} {
		var prev float64 = -1
		buckets := 0
		for _, line := range strings.Split(page, "\n") {
			if !strings.HasPrefix(line, histogram+"_bucket{") {
				continue
			}
			v, err := strconv.ParseFloat(line[strings.LastIndex(line, " ")+1:], 64)
			if err != nil || v < prev {
				t.Fatalf("%s: bucket line %q is not cumulative", histogram, line)
			}
			prev = v
			buckets++
			if strings.Contains(line, `le="+Inf"`) && buckets < 2 {
				t.Fatalf("%s: +Inf comes first", histogram)
			}
		}
		if buckets < 2 || !strings.Contains(page, histogram+`_bucket{model_name="qwen3-32b",le="+Inf"}`) {
			t.Errorf("%s: %d buckets, +Inf missing", histogram, buckets)
		}
		if !strings.Contains(page, histogram+`_sum{model_name="qwen3-32b"}`) || !strings.Contains(page, histogram+`_count{model_name="qwen3-32b"}`) {
			t.Errorf("%s: no _sum/_count", histogram)
		}
	}
}

func TestHistogramObserve(t *testing.T) {
	h := newHistogram([]float64{0.1, 0.5, 1})
	h.observe(0.05, 1)
	h.observe(0.3, 2)
	h.observe(5, 1)
	want := []float64{1, 3, 3, 4}
	for i, c := range h.counts {
		if c != want[i] {
			t.Fatalf("counts %v, want %v", h.counts, want)
		}
	}
	if h.sum != 0.05+0.6+5 {
		t.Fatalf("sum %v", h.sum)
	}
	var b strings.Builder
	h.write(&b, "x", `model_name="m",`)
	for _, line := range []string{`x_bucket{model_name="m",le="0.1"} 1`, `x_bucket{model_name="m",le="+Inf"} 4`, `x_sum{model_name="m"} 5.65`, `x_count{model_name="m"} 4`} {
		if !strings.Contains(b.String(), line+"\n") {
			t.Errorf("no %q in\n%s", line, b.String())
		}
	}
}

func TestEngineStatsObserve(t *testing.T) {
	s := newEngineStats()
	before := s.succeeded
	s.observe(request{prompt: 10, cached: 4, generated: 3, ttft: 200 * time.Millisecond, tpot: 10 * time.Millisecond, e2e: time.Second})
	if s.succeeded != before+1 || s.hits > s.queries {
		t.Fatalf("succeeded %v -> %v, hits %v queries %v", before, s.succeeded, s.hits, s.queries)
	}
}

func TestReplyTokens(t *testing.T) {
	cases := []struct {
		answer    string
		max       int
		ignoreEOS bool
		want      string
	}{
		{"你好世界", 0, false, "你好世界"},
		{"你好世界", 2, false, "你好"},
		{"你好世界", 9, false, "你好世界"},
		{"ab", 5, true, "ababa"},
		{"abcd", 2, true, "ab"},
		{"", 4, true, ""},
	}
	for _, c := range cases {
		if got := string(replyTokens(c.answer, c.max, c.ignoreEOS)); got != c.want {
			t.Errorf("replyTokens(%q, %d, %v) = %q, want %q", c.answer, c.max, c.ignoreEOS, got, c.want)
		}
	}
}

func TestPromptWords(t *testing.T) {
	raw := json.RawMessage(`[{"role":"system","content":"a b"},{"role":"user","content":"c d e"},{"role":"user","content":[{"type":"text","text":"x"}]}]`)
	if n := promptWords(raw); n != 5 {
		t.Fatalf("promptWords = %d", n)
	}
	if n := promptWords(json.RawMessage(`null`)); n != 1 {
		t.Fatalf("empty prompt counts %d", n)
	}
}

func TestPace(t *testing.T) {
	small, smallTok := pace("qwen3-8b-chat")
	big, bigTok := pace("Qwen/Qwen3-32B")
	mid, _ := pace("qwen3.8-27b")
	if !(small < big && smallTok < bigTok && mid == big) {
		t.Fatalf("pace: 8b %v/%v, 32b %v/%v, 27b %v", small, smallTok, big, bigTok, mid)
	}
}
