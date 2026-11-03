package selflog

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureHandler stands in for the process's real handler (stderr/rotated
// file): it records what it received, so the test can prove the collector
// never steals records from the wrapped handler.
type captureHandler struct {
	records []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.records = append(h.records, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func newTestLogger(t *testing.T, queue int, inner *captureHandler) (*slog.Logger, *Collector) {
	t.Helper()
	collector := NewCollector(inner, slog.LevelWarn, queue)
	return slog.New(collector), collector
}

func TestCollectorCapturesWarnAndErrorButNotInfo(t *testing.T) {
	inner := &captureHandler{}
	log, collector := newTestLogger(t, 16, inner)

	log.Info("routine startup")
	log.Warn("disk pressure", "unit", "docker")
	log.Error("probe failed", "err", "boom")

	drained := collector.Drain(16)
	if len(drained) != 2 {
		t.Fatalf("drained %d records, want warn+error: %+v", len(drained), drained)
	}
	if drained[0].Level != "warn" || !strings.Contains(drained[0].Line, "disk pressure") || !strings.Contains(drained[0].Line, "unit=docker") {
		t.Fatalf("warn record wrong: %+v", drained[0])
	}
	if drained[1].Level != "error" || !strings.Contains(drained[1].Line, "probe failed") || !strings.Contains(drained[1].Line, "err=boom") {
		t.Fatalf("error record wrong: %+v", drained[1])
	}
	// The wrapped handler still saw all three records — self-log capture must
	// not change where the process's own logs go.
	if len(inner.records) != 3 {
		t.Fatalf("inner handler saw %d records, want 3", len(inner.records))
	}
	if collector.Dropped() != 0 {
		t.Fatalf("dropped = %d on an empty queue", collector.Dropped())
	}
	if more := collector.Drain(16); len(more) != 0 {
		t.Fatalf("drain is not destructive: %+v", more)
	}
}

func TestCollectorDropsOnOverflowAndCounts(t *testing.T) {
	inner := &captureHandler{}
	log, collector := newTestLogger(t, 2, inner)
	for i := 0; i < 5; i++ {
		log.Warn("storm line", "i", i)
	}
	if got := collector.Dropped(); got != 3 {
		t.Fatalf("dropped = %d, want 3 (queue 2, written 5)", got)
	}
	if got := len(collector.Drain(16)); got != 2 {
		t.Fatalf("drained %d, want the 2 queued lines", got)
	}
	// The wrapped handler was never backpressured: all five reached it.
	if len(inner.records) != 5 {
		t.Fatalf("inner handler saw %d records, want 5", len(inner.records))
	}
}

func TestCollectorDerivedHandlersShareTheQueue(t *testing.T) {
	inner := &captureHandler{}
	collector := NewCollector(inner, slog.LevelWarn, 16)
	log := slog.New(collector).With("component", "uplink")

	log.Warn("reconnect loop", "attempt", 3)

	drained := collector.Drain(16)
	if len(drained) != 1 {
		t.Fatalf("derived handler bypassed the collector: %+v", drained)
	}
	if !strings.Contains(drained[0].Line, "component=uplink") || !strings.Contains(drained[0].Line, "attempt=3") {
		t.Fatalf("attrs missing from the captured line: %+v", drained[0])
	}
}

// The wrapped handler's own level still decides what it writes: the agent's
// file handler set to info must not receive debug records just because the
// collector sits in front of it, and a warn record the queue takes must not
// reach a wrapped handler set to error.
func TestCollectorKeepsTheWrappedHandlersLevel(t *testing.T) {
	var file strings.Builder
	collector := NewCollector(slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelInfo}), slog.LevelWarn, 16)
	log := slog.New(collector)
	log.Debug("runtime command", "args", "image inspect")
	log.Info("agent starting")
	log.Warn("probe failed")
	if strings.Contains(file.String(), "runtime command") || !strings.Contains(file.String(), "agent starting") || !strings.Contains(file.String(), "probe failed") {
		t.Fatalf("info-level file got:\n%s", file.String())
	}
	if !log.Enabled(context.Background(), slog.LevelInfo) || log.Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("Enabled must follow the wrapped handler below the queue's level")
	}

	file.Reset()
	strict := NewCollector(slog.NewTextHandler(&file, &slog.HandlerOptions{Level: slog.LevelError}), slog.LevelWarn, 16)
	slog.New(strict).Warn("queued only")
	if file.Len() != 0 {
		t.Fatalf("error-level file got %q", file.String())
	}
	if got := strict.Drain(4); len(got) != 1 || !strings.Contains(got[0].Line, "queued only") {
		t.Fatalf("queue = %+v", got)
	}
}
