package imagepull

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestCrawlWatchJudgesWholeWindows(t *testing.T) {
	const minRate = 1000
	start := time.Unix(1_000, 0)
	w := crawlWatch{window: time.Minute, minRate: minRate}

	if _, crawling := w.observe(start, 0, true); crawling {
		t.Fatal("the first beat only opens the window")
	}
	if _, crawling := w.observe(start.Add(59*time.Second), 10, true); crawling {
		t.Fatal("judged before the window ended")
	}
	rate, crawling := w.observe(start.Add(time.Minute), 30_000, true)
	// 30000 B over 60 s is 500 B/s, under the 1000 B/s floor.
	if !crawling || rate != 500 {
		t.Fatalf("rate %d over a full window was not a crawl (crawling=%v)", rate, crawling)
	}
	// The next window starts where the last one ended, and a fast one passes.
	if _, crawling := w.observe(start.Add(90*time.Second), 30_100, true); crawling {
		t.Fatal("judged inside the second window")
	}
	if rate, crawling := w.observe(start.Add(2*time.Minute), 30_000+600_000, true); crawling || rate != 10_000 {
		t.Fatalf("a fast window: rate=%d crawling=%v", rate, crawling)
	}
}

func TestCrawlWatchRestartsWhereItCannotJudge(t *testing.T) {
	start := time.Unix(1_000, 0)
	w := crawlWatch{window: time.Minute, minRate: 1000}
	w.observe(start, 0, true)
	// Time spent unpacking (not judged) must not count as a slow download.
	w.observe(start.Add(50*time.Second), 0, false)
	if _, crawling := w.observe(start.Add(100*time.Second), 0, true); crawling {
		t.Fatal("the window did not restart after an unjudged beat")
	}
	if _, crawling := w.observe(start.Add(159*time.Second), 0, true); crawling {
		t.Fatal("judged before the restarted window ended")
	}
	if _, crawling := w.observe(start.Add(161*time.Second), 0, true); !crawling {
		t.Fatal("a window with no bytes was not a crawl")
	}
	// A layer restarting makes the total fall back: start over, no negative rate.
	w = crawlWatch{window: time.Minute, minRate: 1000}
	w.observe(start, 5_000_000, true)
	if rate, crawling := w.observe(start.Add(time.Minute), 100, true); crawling || rate != 0 {
		t.Fatalf("a restarted layer was judged: rate=%d crawling=%v", rate, crawling)
	}
}

// slowPull downloads mbPerMinute MB of a layer for every virtual minute, for
// minutes minutes, then succeeds unless it was cancelled meanwhile.
func slowPull(c *clock, mbPerMinute, minutes int) pullStep {
	return func(ctx context.Context, onLine func(string)) error {
		for minute := 1; minute <= minutes; minute++ {
			onLine(fmt.Sprintf("abcdef123456: Downloading %dMB/1000000MB", minute*mbPerMinute))
			time.Sleep(15 * time.Millisecond) // several watch ticks see the line
			c.Advance(time.Minute)
			time.Sleep(15 * time.Millisecond)
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestWalkMovesOnFromASourceThatCrawls(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	// 1 MB a minute never stalls (the bytes keep changing) yet is ~17 KB/s.
	d := newDriver("udocker", map[string]pullStep{
		"a.example/x:1": slowPull(c, 1, 30),
		"b.example/x:1": ok,
	})
	var failures []error
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Failed: func(_ string, err error) { failures = append(failures, err) },
	})
	if err != nil || res.Ref != "b.example/x:1" {
		t.Fatalf("walk = %+v, %v; want the second source", res, err)
	}
	if len(failures) != 1 || !errors.Is(failures[0], ErrCrawling) || errors.Is(failures[0], ErrStalled) {
		t.Fatalf("failures = %v, want one crawl", failures)
	}
	if msg := failures[0].Error(); !strings.Contains(msg, "KiB/s over 5m0s, below 512 KiB/s") {
		t.Fatalf("the failure does not say how slow it was: %q", msg)
	}
	if !p.prober.Cooled("a.example") {
		t.Fatal("a crawling host must cool down")
	}
}

func TestWalkLeavesTheLastSourceAloneEvenIfItCrawls(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	// Twelve virtual minutes at ~17 KB/s: far past one window, nowhere left to go.
	d := newDriver("udocker", map[string]pullStep{
		"a.example/x:1": fail("connection refused"),
		"b.example/x:1": slowPull(c, 1, 12),
	})
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
	if err != nil || res.Ref != "b.example/x:1" {
		t.Fatalf("the last source was given up on: %+v, %v", res, err)
	}
	if p.prober.Cooled("b.example") {
		t.Fatal("the source that finished was cooled")
	}
}

func TestWalkKeepsASourceThatIsFastEnough(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	// 600 000 MB over ten minutes is ~1 GB/s; a second source is waiting.
	d := newDriver("udocker", map[string]pullStep{
		"a.example/x:1": slowPull(c, 60_000, 10),
		"b.example/x:1": ok,
	})
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{})
	if err != nil || res.Ref != "a.example/x:1" {
		t.Fatalf("a healthy first source was abandoned: %+v, %v", res, err)
	}
}

func TestWalkDoesNotJudgeACoarseCounterAsACrawl(t *testing.T) {
	shortTimings(t)
	c := newClock()
	StallTimeout = 3 * time.Minute
	p, _ := newPuller(t, c, nil, Config{})
	const step = 100 << 20
	// nerdctl shows 0.1 GiB steps: one step every two virtual minutes is
	// ~850 KB/s, but the count alone cannot be told from a crawl.
	coarse := func(ctx context.Context, onLine func(string)) error {
		for n := int64(1); n <= 6; n++ {
			onLine(runtime.PullLineDownloadingInSteps("abcdef123456", n*step, 100<<30, step))
			time.Sleep(15 * time.Millisecond)
			c.Advance(2 * time.Minute)
			time.Sleep(15 * time.Millisecond)
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return nil
	}
	d := newDriver("nerdctl", map[string]pullStep{"a.example/x:1": coarse, "b.example/x:1": ok})
	var failed atomic.Int32
	res, err := p.Walk(context.Background(), d, cands("a.example/x:1", "b.example/x:1"), Options{}, Hooks{
		Failed: func(string, error) { failed.Add(1) },
	})
	if err != nil || res.Ref != "a.example/x:1" || failed.Load() != 0 {
		t.Fatalf("a coarse counter was taken for a crawl: %+v, %v, failures=%d", res, err, failed.Load())
	}
}
