package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/conn"
	"gpuworks.ai/gpuworks/internal/proto"
)

// Only connection evidence is aggregated. Keep three fixed lane slots rather
// than a growing map keyed by remote data. The same state survives reconnects.
type connectionEventGate struct {
	mu    sync.Mutex
	lanes [3]struct {
		key        string
		at         time.Time
		suppressed uint64
	}
}

type connectionEventHandler struct {
	next slog.Handler
	gate *connectionEventGate
}

func connectionDiagnosticLogger(log *slog.Logger) *slog.Logger {
	return slog.New(&connectionEventHandler{next: log.Handler(), gate: &connectionEventGate{}})
}

func (h *connectionEventHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *connectionEventHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message != "connection_event_v1" {
		return h.next.Handle(ctx, r)
	}
	var lane, stage, reason string
	r.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "lane":
			lane = attr.Value.String()
		case "stage":
			stage = attr.Value.String()
		case "reason":
			reason = attr.Value.String()
		}
		return true
	})
	index := 0 // single lane, including accepted server_dial connections
	if lane == "ctrl" {
		index = 1
	} else if lane == "data" {
		index = 2
	}
	key := stage + ":" + reason
	h.gate.mu.Lock()
	slot := &h.gate.lanes[index]
	elapsed := r.Time.Sub(slot.at)
	if slot.key == key && !slot.at.IsZero() && elapsed >= 0 && elapsed < time.Minute {
		slot.suppressed++
		h.gate.mu.Unlock()
		return nil
	}
	if slot.suppressed != 0 {
		r.AddAttrs(slog.Uint64("suppressed_retries", slot.suppressed))
	}
	slot.key, slot.at, slot.suppressed = key, r.Time, 0
	h.gate.mu.Unlock()
	return h.next.Handle(ctx, r)
}

func (h *connectionEventHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &connectionEventHandler{next: h.next.WithAttrs(attrs), gate: h.gate}
}

func (h *connectionEventHandler) WithGroup(name string) slog.Handler {
	return &connectionEventHandler{next: h.next.WithGroup(name), gate: h.gate}
}

// Log only controlled facts: the original error may echo a token, URL or an
// untrusted server rejection detail. Classification remains in conn.
func logConnectionFailure(log *slog.Logger, c *conn.Conn, rejected bool) {
	report := c.ConnectionReport()
	reason := proto.ConnectionValue(report, "connection.reason")
	if reason == "" {
		reason = "transport"
	}
	if rejected {
		log.Error("server rejected connection", "reason", reason)
	} else {
		log.Warn("connection ended", "reason", reason)
	}
}
