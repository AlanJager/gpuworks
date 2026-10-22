package app

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/selflog"
)

// TestRunSelfLogShipperShipsAgentSource pins the self-log contract end to
// end: slog records at warn+ written through the collector reach the TLogs
// stream as source=agent lines with their severity, while the info record
// stays local.
func TestRunSelfLogShipperShipsAgentSource(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, t.TempDir(), "none", metricsOnlyProber{}, nil)

	inner := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	n.selfLog = selflog.NewCollector(inner, slog.LevelWarn, 64)
	logger := slog.New(n.selfLog)

	var mu sync.Mutex
	shipped := map[string]proto.LogLine{}
	n.Bind(func(kind string, body any) error {
		if kind == proto.TLogs {
			mu.Lock()
			for _, l := range body.(proto.LogBatch).Lines {
				shipped[l.Line] = l
			}
			mu.Unlock()
		}
		return nil
	})

	logger.Info("local chatter that must not ship")
	logger.Warn("reconnect loop exhausted", "server", "mirror1")
	logger.Error("bench writer failed", "err", "enospc")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.runSelfLogShipper(ctx, 10*time.Millisecond)

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(shipped)
	}
	deadline := time.After(3 * time.Second)
	for count() < 2 {
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timeout: shipped %d agent lines: %+v", len(shipped), shipped)
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	warn, ok := shipped["reconnect loop exhausted server=mirror1"]
	if !ok {
		t.Fatalf("warn line not shipped: %+v", shipped)
	}
	if warn.Level != "warn" || warn.Source != "agent" || warn.Instance != "" {
		t.Fatalf("warn line = %+v, want source=agent with warn level and no instance", warn)
	}
	errLine, ok := shipped["bench writer failed err=enospc"]
	if !ok || errLine.Level != "error" {
		t.Fatalf("error line = %+v (present=%v)", errLine, ok)
	}
	if strings.Contains(strings.Join(shippedLines(shipped), "|"), "local chatter") {
		t.Fatal("info record must not ship under warn+ collection")
	}
}

func shippedLines(m map[string]proto.LogLine) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
