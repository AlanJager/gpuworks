package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func connectionEvent(at time.Time, lane, stage, reason string) slog.Record {
	r := slog.NewRecord(at, slog.LevelInfo, "connection_event_v1", 0)
	r.AddAttrs(slog.String("source", "agent_runtime"), slog.String("lane", lane), slog.String("stage", stage), slog.String("reason", reason))
	return r
}

func TestConnectionDiagnosticLogsAggregatePerLaneAndKeepTransitions(t *testing.T) {
	var out bytes.Buffer
	log := connectionDiagnosticLogger(slog.New(slog.NewTextHandler(&out, nil)))
	h := log.Handler()
	at := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	for _, r := range []slog.Record{
		connectionEvent(at, "ctrl", "dial", "timeout"),
		connectionEvent(at.Add(20*time.Second), "ctrl", "dial", "timeout"),
		connectionEvent(at.Add(21*time.Second), "data", "dial", "timeout"),
		connectionEvent(at.Add(59*time.Second), "ctrl", "dial", "timeout"),
		connectionEvent(at.Add(time.Minute), "ctrl", "dial", "timeout"),
		connectionEvent(at.Add(61*time.Second), "ctrl", "wait_registered", "bad_token"),
	} {
		if err := h.Handle(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	text := out.String()
	if strings.Count(text, "msg=connection_event_v1") != 4 || !strings.Contains(text, "suppressed_retries=2") || !strings.Contains(text, "reason=bad_token") {
		t.Fatalf("incorrect evidence aggregation:\n%s", text)
	}
	for range 2 {
		log.Warn("another warning", "reason", "timeout")
	}
	if strings.Count(out.String(), `msg="another warning"`) != 2 {
		t.Fatal("connection rate gate changed other warnings")
	}
}

func TestConnectionDiagnosticLogClonesShareBoundedGate(t *testing.T) {
	var out bytes.Buffer
	log := connectionDiagnosticLogger(slog.New(slog.NewTextHandler(&out, nil)))
	at := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	first := log.With("node", "one").Handler()
	second := log.With("node", "two").WithGroup("details").Handler()
	if err := first.Handle(context.Background(), connectionEvent(at, "single", "session", "transport")); err != nil {
		t.Fatal(err)
	}
	if err := second.Handle(context.Background(), connectionEvent(at.Add(time.Second), "single", "session", "transport")); err != nil {
		t.Fatal(err)
	}
	if err := second.Handle(context.Background(), connectionEvent(at.Add(time.Minute), "single", "session", "transport")); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "msg=connection_event_v1") != 2 || !strings.Contains(out.String(), "details.suppressed_retries=1") || !strings.Contains(out.String(), "node=two") {
		t.Fatalf("cloned logger lost attrs, grouping or gate: %s", out.String())
	}
	var wg sync.WaitGroup
	for range 25 {
		wg.Go(func() {
			_ = first.Handle(context.Background(), connectionEvent(at.Add(62*time.Second), "single", "session", "transport"))
		})
	}
	wg.Wait()
	if strings.Count(out.String(), "msg=connection_event_v1") != 2 {
		t.Fatal("concurrent repeat events bypassed the shared gate")
	}
}
