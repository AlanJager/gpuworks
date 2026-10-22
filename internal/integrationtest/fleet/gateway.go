package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// GatewayLoad is an OpenAI client population: Concurrency workers, each
// issuing one streaming chat completion after another through the gateway for
// Duration. Worker i always asks for Models[i%len(Models)], so every deployment
// sees the same offered load and any imbalance inside one deployment is the
// router's doing.
type GatewayLoad struct {
	Base        string
	APIKey      string
	Models      []string
	Concurrency int
	Duration    time.Duration
	// RampUp spreads the workers' first connections over this long, and each
	// worker's first request (the one that opens its connection) is not
	// measured. Opening thousands of connections in the same instant
	// overflows a default listen backlog (somaxconn 128), and the SYN
	// retransmits that follow add a whole second to exactly those requests.
	RampUp time.Duration
	// PromptBytes pads each request's user message to about this size, to
	// model long-context requests (60K English tokens are ~240 KB of text).
	PromptBytes int
}

func (g GatewayLoad) prompt(worker int) string {
	text := fmt.Sprintf("load worker %d", worker)
	if pad := g.PromptBytes - len(text); pad > 0 {
		text += strings.Repeat(" lorem", pad/6+1)[:pad]
	}
	return text
}

// GatewayReport is what the clients saw. TTFT is the time to the first
// streamed line, E2E the time to the end of the stream.
type GatewayReport struct {
	Elapsed   time.Duration
	Requests  int64
	Errors    int64
	Deltas    int64
	TTFT      Latencies
	E2E       Latencies
	mu        sync.Mutex
	failures  map[string]int64
	firstSeen []string
}

func (r *GatewayReport) fail(kind string) {
	atomic.AddInt64(&r.Errors, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failures == nil {
		r.failures = make(map[string]int64)
	}
	if r.failures[kind] == 0 {
		r.firstSeen = append(r.firstSeen, kind)
	}
	r.failures[kind]++
}

// Failures lists each distinct failure with its count, in the order first seen.
func (r *GatewayReport) Failures() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.firstSeen))
	for _, kind := range r.firstSeen {
		out = append(out, fmt.Sprintf("%dx %s", r.failures[kind], kind))
	}
	return out
}

// Run drives the load until Duration has passed; requests already in flight
// at the deadline finish and are counted.
func (g GatewayLoad) Run(ctx context.Context) *GatewayReport {
	report := &GatewayReport{}
	if len(g.Models) == 0 || g.Concurrency <= 0 {
		return report
	}
	transport := &http.Transport{
		MaxIdleConns:        g.Concurrency,
		MaxIdleConnsPerHost: g.Concurrency,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute}
	endpoint := strings.TrimSuffix(g.Base, "/") + "/v1/chat/completions"
	measuring := time.Now().Add(g.RampUp)
	deadline := measuring.Add(g.Duration)
	var wg sync.WaitGroup
	for i := 0; i < g.Concurrency; i++ {
		body, _ := json.Marshal(map[string]any{
			"model":    g.Models[i%len(g.Models)],
			"messages": []map[string]string{{"role": "user", "content": g.prompt(i)}},
			"stream":   true,
		})
		delay := g.RampUp * time.Duration(i) / time.Duration(g.Concurrency)
		wg.Add(1)
		go func() {
			defer wg.Done()
			time.Sleep(delay)
			g.once(ctx, client, endpoint, body, report, false)
			for ctx.Err() == nil && time.Now().Before(deadline) {
				g.once(ctx, client, endpoint, body, report, true)
			}
		}()
	}
	wg.Wait()
	report.Elapsed = time.Since(measuring)
	return report
}

// Warm sends exactly one request to each model, concurrently, so a case can
// prove every deployment routable before it starts the clock.
func (g GatewayLoad) Warm(ctx context.Context) *GatewayReport {
	report := &GatewayReport{}
	client := &http.Client{Timeout: 5 * time.Minute}
	endpoint := strings.TrimSuffix(g.Base, "/") + "/v1/chat/completions"
	start := time.Now()
	var wg sync.WaitGroup
	for _, model := range g.Models {
		body, _ := json.Marshal(map[string]any{
			"model":    model,
			"messages": []map[string]string{{"role": "user", "content": "warm-up"}},
			"stream":   true,
		})
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.once(ctx, client, endpoint, body, report, true)
		}()
	}
	wg.Wait()
	report.Elapsed = time.Since(start)
	return report
}

// once sends one request. An unmeasured request still reports failures, so a
// connection that cannot be opened is never hidden, but its latency and its
// place in the request count are left out.
func (g GatewayLoad) once(ctx context.Context, client *http.Client, endpoint string, body []byte, report *GatewayReport, measure bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		report.fail("build request: " + err.Error())
		return
	}
	req.Header.Set("Authorization", "Bearer "+g.APIKey)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := client.Do(req)
	if measure {
		atomic.AddInt64(&report.Requests, 1)
	}
	if err != nil {
		report.fail("transport: " + errorClass(err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		report.fail(fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(payload))))
		return
	}
	reader := bufio.NewReader(resp.Body)
	first := true
	done := false
	for {
		line, err := reader.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			if first && measure {
				report.TTFT.Add(time.Since(start))
			}
			first = false
			if measure && strings.Contains(line, `"delta":{"content"`) {
				atomic.AddInt64(&report.Deltas, 1)
			}
			if strings.TrimSpace(line) == "data: [DONE]" {
				done = true
			}
		}
		if err != nil {
			if err != io.EOF {
				report.fail("stream read: " + errorClass(err))
				return
			}
			break
		}
	}
	if !done {
		report.fail("stream ended without [DONE]")
		return
	}
	if measure {
		report.E2E.Add(time.Since(start))
	}
}

// errorClass strips per-connection detail (ports, addresses) so identical
// failures group into one line.
func errorClass(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.Contains(msg, "127.0.0.1") {
		msg = msg[i+2:]
	}
	return msg
}

// Lines renders the client side of the run.
func (r *GatewayReport) Lines(ttft, tpot time.Duration, deltas int) []string {
	completed := r.E2E.Count()
	seconds := r.Elapsed.Seconds()
	ideal := ttft + time.Duration(deltas)*tpot
	lines := []string{
		fmt.Sprintf("requests=%d completed=%d errors=%d in %s", r.Requests, completed, r.Errors, r.Elapsed.Round(time.Millisecond)),
		fmt.Sprintf("throughput: %.0f req/s, %.0f streamed deltas/s", float64(completed)/seconds, float64(r.Deltas)/seconds),
		fmt.Sprintf("TTFT p50=%s p95=%s p99=%s (engine first-token delay %s)",
			r.TTFT.Percentile(0.5).Round(time.Millisecond), r.TTFT.Percentile(0.95).Round(time.Millisecond),
			r.TTFT.Percentile(0.99).Round(time.Millisecond), ttft),
		fmt.Sprintf("E2E  p50=%s p95=%s p99=%s (engine-only time %s)",
			r.E2E.Percentile(0.5).Round(time.Millisecond), r.E2E.Percentile(0.95).Round(time.Millisecond),
			r.E2E.Percentile(0.99).Round(time.Millisecond), ideal),
	}
	for _, failure := range r.Failures() {
		lines = append(lines, "failure: "+failure)
	}
	return lines
}

// RouteSpread is how evenly the gateway spread one deployment's requests over
// its replicas, from the proxy's own gpuworks_proxy_routed_total counters.
type RouteSpread struct {
	Deployment string
	Replicas   int
	Min, Max   float64
	Mean       float64
}

// Imbalance is the largest relative distance of a replica from the mean.
func (s RouteSpread) Imbalance() float64 {
	if s.Mean == 0 {
		return math.Inf(1)
	}
	return math.Max(s.Max-s.Mean, s.Mean-s.Min) / s.Mean
}

// seriesTotals reads a cumulative counter's series over the last window;
// the largest point of each series is its total.
func seriesTotals(client *Client, metric string, window time.Duration) ([]apiwire.MetricSeries, []float64, error) {
	var list apiwire.List[apiwire.MetricSeries]
	path := fmt.Sprintf("/api/v1/metrics/query?metric=%s&range=%s&step=10s", metric, window)
	if err := client.GetJSON(path, &list); err != nil {
		return nil, nil, err
	}
	series := list.Items
	totals := make([]float64, len(series))
	for i, s := range series {
		for _, p := range s.Points {
			totals[i] = math.Max(totals[i], p.Value)
		}
	}
	return series, totals, nil
}

// SumMetric adds up a cumulative counter across all its series.
func SumMetric(client *Client, metric string, window time.Duration) (float64, error) {
	_, totals, err := seriesTotals(client, metric, window)
	sum := 0.0
	for _, v := range totals {
		sum += v
	}
	return sum, err
}

// RoutedSpread reads the proxy's per-instance routing counters for the last
// window and summarises each deployment.
func RoutedSpread(client *Client, window time.Duration) ([]RouteSpread, error) {
	series, totals, err := seriesTotals(client, "gpuworks_proxy_routed_total", window)
	if err != nil {
		return nil, err
	}
	perDeployment := map[string][]float64{}
	for i, s := range series {
		dep := s.Labels["deployment"]
		perDeployment[dep] = append(perDeployment[dep], totals[i])
	}
	out := make([]RouteSpread, 0, len(perDeployment))
	for dep, totals := range perDeployment {
		spread := RouteSpread{Deployment: dep, Replicas: len(totals), Min: math.Inf(1)}
		sum := 0.0
		for _, v := range totals {
			spread.Min = math.Min(spread.Min, v)
			spread.Max = math.Max(spread.Max, v)
			sum += v
		}
		spread.Mean = sum / float64(len(totals))
		out = append(out, spread)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Imbalance() > out[j].Imbalance() })
	return out, nil
}
