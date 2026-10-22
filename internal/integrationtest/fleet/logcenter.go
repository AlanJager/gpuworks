package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/testenv/apiwire"
)

// The log-center scale driver measures the read side of the log center while
// the fleet writes: first-screen tails, keyword search, histograms, and how
// fresh the newest searchable line is while agents stream logs through the
// same control plane the operator is querying. Both fleet shapes (in-process
// and the l2 two-process shape) run the same sampler so their numbers compare.

// LogReadReport aggregates one sampling window.
type LogReadReport struct {
	Samples   int
	First     *Latencies // GET /api/v1/logs/search (the /logs first screen)
	Keyword   *Latencies // GET /api/v1/logs/search?q="CUDA out of memory"
	Histogram *Latencies // GET /api/v1/logs/histogram
	Freshness *Latencies // now - newest TimeMS in the first screen
	Searches  int64      // first-screen pages served during the window
	NewestMS  int64      // newest line the last successful first screen saw
	// WireError is the first response body that did not decode as the
	// documented wire shape; a non-empty value fails the case, since the
	// freshness verdict would otherwise go vacuously unsampled.
	WireError string
}

func (r *LogReadReport) noteWireError(what string, err error) {
	if r.WireError == "" {
		r.WireError = what + ": " + err.Error()
	}
}

// Lines renders the window the way the fleet report does, so both shapes
// print comparable lines.
func (r LogReadReport) Lines() []string {
	med := func(l *Latencies) string {
		if l.Count() == 0 {
			return "not sampled"
		}
		return fmt.Sprintf("p50=%s p95=%s (n=%d)", l.Percentile(0.5).Round(time.Millisecond), l.Percentile(0.95).Round(time.Millisecond), l.Count())
	}
	fresh := "not sampled"
	if r.Freshness.Count() > 0 {
		fresh = fmt.Sprintf("p50=%s p95=%s", r.Freshness.Percentile(0.5).Round(time.Millisecond), r.Freshness.Percentile(0.95).Round(time.Millisecond))
	}
	return []string{
		fmt.Sprintf("first screen: %s", med(r.First)),
		fmt.Sprintf("keyword search: %s", med(r.Keyword)),
		fmt.Sprintf("histogram: %s", med(r.Histogram)),
		fmt.Sprintf("ingest freshness (now - newest searchable line): %s", fresh),
		fmt.Sprintf("first-screen pages served during the window: %d", r.Searches),
	}
}

// SampleLogReads queries the log-center read endpoints for the window and
// records latencies. It returns early only when the client fails outright;
// slow reads are the result being measured.
func SampleLogReads(ctx context.Context, c *Client, window time.Duration) LogReadReport {
	report := LogReadReport{
		First:     &Latencies{},
		Keyword:   &Latencies{},
		Histogram: &Latencies{},
		Freshness: &Latencies{},
	}
	// The console defaults to an explicit 24h range; the sampler mirrors it so the
	// measured latencies are the product's, not an API-only worst case.
	fromMS := time.Now().Add(-24 * time.Hour).UnixMilli()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return report
		default:
		}
		start := time.Now()
		body, err := getBody(c, fmt.Sprintf("/api/v1/logs/search?limit=200&from=%d", fromMS))
		latency := time.Since(start)
		if err != nil {
			report.First.Add(latency)
			report.Samples++
			continue
		}
		report.First.Add(latency)
		report.Searches++
		var page apiwire.LogsPage
		if err := json.Unmarshal(body, &page); err != nil {
			report.noteWireError("first screen", err)
		} else if len(page.Items) > 0 {
			report.NewestMS = page.Items[0].TimeMS
			if lag := time.Since(time.UnixMilli(page.Items[0].TimeMS)); lag > 0 {
				report.Freshness.Add(lag)
			}
		}

		start = time.Now()
		keyword, err := getBody(c, `/api/v1/logs/search?q=`+url.QueryEscape(`"CUDA out of memory"`)+fmt.Sprintf(`&from=%d&limit=200`, fromMS))
		report.Keyword.Add(time.Since(start))
		if err == nil {
			if err := json.Unmarshal(keyword, &apiwire.LogsPage{}); err != nil {
				report.noteWireError("keyword search", err)
			}
		}

		start = time.Now()
		histogram, err := getBody(c, "/api/v1/logs/histogram?bucket=60000")
		report.Histogram.Add(time.Since(start))
		if err == nil {
			if _, err := apiwire.DecodeList[apiwire.LogsBucket](histogram); err != nil {
				report.noteWireError("histogram", err)
			}
		}

		report.Samples++
		sleep(ctx, time.Second)
	}
	return report
}

func getBody(c *Client, path string) ([]byte, error) {
	resp, err := c.Get(path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return body, nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// WaitInstancesRunning polls the instance list until at least want instances
// report running, or the timeout expires (last count in the error).
func WaitInstancesRunning(c *Client, want int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	last := -1
	for time.Now().Before(deadline) {
		var instances apiwire.List[struct {
			State string `json:"state"`
		}]
		if err := c.GetJSON("/api/v1/instances", &instances); err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		last = 0
		for _, i := range instances.Items {
			if i.State == "running" {
				last++
			}
		}
		if last >= want {
			return last, nil
		}
		time.Sleep(2 * time.Second)
	}
	return last, fmt.Errorf("only %d of %d instances running when the timeout expired", last, want)
}

// SortDurations is a small helper for report rendering.
func SortDurations(d []time.Duration) { sort.Slice(d, func(i, j int) bool { return d[i] < d[j] }) }

// Rate log stream: the fake driver's Logs hook hands one of these to the
// agent's follower, and the generator writes synthetic instance lines at a
// fixed rate until the reader goes away (a container restart closes the
// agent's reader, the next write fails, the generator exits).
type rateLogStream struct{ pr *io.PipeReader }

func (s *rateLogStream) Read(p []byte) (int, error) { return s.pr.Read(p) }

func (s *rateLogStream) Close() error { return s.pr.Close() }

// NewRateLogStream exposes the rate-limited synthetic log stream to the
// two-process fleet host, which injects it through the same fake-driver
// hook the in-process shape uses.
func NewRateLogStream(perSec float64, container string, follow bool, tail int) io.ReadCloser {
	return newRateLogStream(perSec, container, follow, tail)
}

// newRateLogStream mirrors docker-logs semantics: follow=true streams
// synthetic lines at the rate forever (until the reader goes away), while
// follow=false is the bounded snapshot callers like the health probe read
// with io.ReadAll — an endless stream there would hang deployment.
func newRateLogStream(perSec float64, container string, follow bool, tail int) *rateLogStream {
	if perSec < 0.01 {
		perSec = 0.01
	}
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		if !follow {
			n := tail
			if n <= 0 || n > 200 {
				n = 50
			}
			for i := 0; i < n; i++ {
				if _, err := pw.Write([]byte(syntheticInstanceLine(i, container) + "\n")); err != nil {
					return
				}
			}
			return
		}
		interval := time.Duration(float64(time.Second) / perSec)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for i := 0; ; i++ {
			<-ticker.C
			if _, err := pw.Write([]byte(syntheticInstanceLine(i, container) + "\n")); err != nil {
				return
			}
		}
	}()
	return &rateLogStream{pr: pr}
}

// syntheticInstanceLine mirrors real engine output: mostly INFO request and
// access chatter on application endpoints (which must survive the noise
// filter), a WARN band, and a CUDA OOM error tail the keyword sample can
// match.
func syntheticInstanceLine(i int, container string) string {
	switch p := i % 20; {
	case p == 7:
		return fmt.Sprintf("(APIServer pid=%d) WARNING 09-18 10:%02d:%02d [config.py:%d] Unknown vLLM environment variable detected: VLLM_%d", i%100, i%60, i%60, 100+i%50, i%7)
	case p == 13:
		return fmt.Sprintf("(APIServer pid=%d) ERROR 09-18 10:%02d:%02d [core.py:%d] CUDA out of memory during batch %d", i%100, i%60, i%60, 900+i%99, i)
	default:
		return fmt.Sprintf(`(APIServer pid=%d) INFO:     127.0.0.1:%d - "POST /v1/completions HTTP/1.1" 200 OK [%s]`, i%100, 30000+i%20000, container)
	}
}

// PercentileOf exists for cases that collect raw durations; Latencies already
// covers the sampler.
func PercentileOf(d []time.Duration, p float64) time.Duration {
	SortDurations(d)
	if len(d) == 0 {
		return 0
	}
	idx := int(math.Round(p * float64(len(d)-1)))
	return d[idx]
}
