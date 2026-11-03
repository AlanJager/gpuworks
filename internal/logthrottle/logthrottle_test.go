package logthrottle

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) advance(d time.Duration) { c.now = c.now.Add(d) }

func newLogger(out io.Writer, c *clock, level slog.Level) (*slog.Logger, *Handler) {
	h := New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}), Options{Identity: []string{"code"}, Now: c.Now})
	return slog.New(h), h
}

func count(out *strings.Builder, substr string) int { return strings.Count(out.String(), substr) }

func TestHandlerLimitsEachKindPerWindowAndSummarizes(t *testing.T) {
	var out strings.Builder
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	log, _ := newLogger(&out, c, slog.LevelInfo)
	for i := 0; i < 1000; i++ {
		log.Info("api error", "code", "AUTHENTICATION_REQUIRED", "path", fmt.Sprintf("/api/v1/x/%d", i))
		log.Info("api error", "code", "DEPLOYMENT_NEVER_FITS", "path", "/api/v1/deployments")
	}
	if got := count(&out, "code=AUTHENTICATION_REQUIRED"); got != 10 {
		t.Fatalf("AUTHENTICATION_REQUIRED written %d times, want 10:\n%s", got, out.String())
	}
	if got := count(&out, "code=DEPLOYMENT_NEVER_FITS"); got != 10 {
		t.Fatalf("a different code is another kind; written %d times", got)
	}
	if count(&out, SuppressedMessage) != 0 {
		t.Fatal("summary before the window ended")
	}

	// The next record after the window carries the summary of the last one.
	c.advance(30 * time.Second)
	out.Reset()
	log.Info("api error", "code", "AUTHENTICATION_REQUIRED", "path", "/later")
	if !strings.Contains(out.String(), `msg="suppressed similar log messages" message="api error" suppressed=990`) ||
		!strings.Contains(out.String(), `window=30s code=AUTHENTICATION_REQUIRED`) || !strings.Contains(out.String(), "path=/later") {
		t.Fatalf("after the window:\n%s", out.String())
	}
	if count(&out, "message=\"api error\" suppressed=990") != 2 {
		t.Fatalf("each kind gets its own summary:\n%s", out.String())
	}
}

func TestSweepSummarizesAFloodThatStopped(t *testing.T) {
	var out strings.Builder
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	log, h := newLogger(&out, c, slog.LevelInfo)
	for i := 0; i < 25; i++ {
		log.Warn("agent session failed", "reason", "bad_token")
	}
	h.Sweep(context.Background())
	if count(&out, SuppressedMessage) != 0 {
		t.Fatal("swept before the window ended")
	}
	c.advance(31 * time.Second)
	out.Reset()
	h.Sweep(context.Background())
	if !strings.Contains(out.String(), `level=WARN msg="suppressed similar log messages" message="agent session failed" suppressed=15`) {
		t.Fatalf("sweep:\n%s", out.String())
	}
	out.Reset()
	c.advance(31 * time.Second)
	h.Sweep(context.Background())
	if out.Len() != 0 {
		t.Fatalf("a kind is summarized once: %s", out.String())
	}
}

func TestDebugSuppressesOnlyExactRepeats(t *testing.T) {
	var out strings.Builder
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	log, _ := newLogger(&out, c, slog.LevelDebug)
	for i := 0; i < 200; i++ {
		log.Debug("runtime command", "args", fmt.Sprintf("image inspect img:%d", i))
	}
	if got := count(&out, `msg="runtime command"`); got != 200 {
		t.Fatalf("distinct debug records are each written; got %d", got)
	}
	out.Reset()
	for i := 0; i < 200; i++ {
		log.Debug("runtime command", "args", "image inspect same")
	}
	if got := count(&out, `msg="runtime command"`); got != 50 {
		t.Fatalf("exact debug repeats: %d written, want 50", got)
	}
}

func TestLoggerAttributesSeparateKinds(t *testing.T) {
	var out strings.Builder
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	log, _ := newLogger(&out, c, slog.LevelInfo)
	a, b := log.With("component", "hub"), log.With("component", "proxy")
	for i := 0; i < 20; i++ {
		a.Info("retry")
		b.Info("retry")
	}
	if count(&out, "component=hub") != 10 || count(&out, "component=proxy") != 10 {
		t.Fatalf("got:\n%s", out.String())
	}
}

// Distinct kinds without end (random paths) cannot walk around the bound:
// beyond it they count against their message alone.
func TestKindsBeyondTheBoundCountByMessage(t *testing.T) {
	var out strings.Builder
	c := &clock{now: time.Unix(1_800_000_000, 0)}
	h := New(slog.NewTextHandler(&out, nil), Options{Identity: []string{"path"}, MaxKinds: 2, Now: c.Now})
	log := slog.New(h)
	for i := 0; i < 1000; i++ {
		log.Info("http request", "path", fmt.Sprintf("/random/%d", i))
	}
	if got := count(&out, `msg="http request"`); got != 12 {
		t.Fatalf("2 tracked kinds plus 10 of the coarse one, got %d", got)
	}
	c.advance(30 * time.Second)
	out.Reset()
	h.Sweep(context.Background())
	if !strings.Contains(out.String(), `message="http request" suppressed=988`) {
		t.Fatalf("summary: %s", out.String())
	}
}

func TestHandlerIsSafeForConcurrentUse(t *testing.T) {
	h := New(slog.NewTextHandler(io.Discard, nil), Options{Identity: []string{"code"}})
	log := slog.New(h)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				log.Info("api error", "code", fmt.Sprint(i%7), "g", g)
				if i%500 == 0 {
					h.Sweep(context.Background())
				}
			}
		}(g)
	}
	wg.Wait()
}

// The cost of throttling next to formatting the record it saves.
func BenchmarkThrottledInfo(b *testing.B) {
	for _, tc := range []struct {
		name  string
		build func() *slog.Logger
	}{
		{"text-handler-alone", func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }},
		{"throttled-distinct-kinds", func() *slog.Logger {
			return slog.New(New(slog.NewTextHandler(io.Discard, nil), Options{Identity: []string{"code"}, Limit: 1 << 30}))
		}},
		{"throttled-flood", func() *slog.Logger {
			return slog.New(New(slog.NewTextHandler(io.Discard, nil), Options{Identity: []string{"code"}}))
		}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			log := tc.build()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				log.Info("api error", "code", "AUTHENTICATION_REQUIRED", "method", "GET", "path", "/api/v1/nodes", "request_id", "4f3c2a1b")
			}
		})
	}
}
