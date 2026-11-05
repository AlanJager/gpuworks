package debuglog

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/logthrottle"
	"gpuworks.ai/gpuworks/internal/selflog"
)

func TestLoggerSessionCapturesBeforeThrottle(t *testing.T) {
	var ordinary strings.Builder
	now := time.Unix(1800000000, 0)
	collector := selflog.NewCollector(slog.NewTextHandler(&ordinary, &slog.HandlerOptions{Level: slog.LevelInfo}), slog.LevelWarn, 512)
	log, tee, throttle := NewLogger(collector, logthrottle.Options{Now: func() time.Time { return now }, Identity: []string{"code"}})
	tee.SetClock(func() time.Time { return now })
	log = log.With("component", "proxy")
	path := filepath.Join(t.TempDir(), "session.log")
	if e := tee.Start("session", path, now.Add(time.Hour), 1<<20); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 100; i++ {
		log.Warn("repeated warning", "code", "UPSTREAM_EOF", "request_id", i)
		log.Debug("repeated debug", "step", "read")
	}
	now = now.Add(31 * time.Second)
	throttle.Sweep(context.Background())
	tee.Stop("")
	session := read(t, path)
	if got := strings.Count(session, `level=WARN msg="repeated warning"`); got != 100 {
		t.Errorf("raw session warnings=%d, want100", got)
	}
	if got := strings.Count(session, `level=DEBUG msg="repeated debug"`); got != 100 {
		t.Errorf("raw session debug=%d, want100", got)
	}
	if !strings.Contains(session, "component=proxy") {
		t.Error("session lost WithAttrs")
	}
	center := collector.Drain(512)
	if len(center) != 11 || collector.Dropped() != 0 {
		t.Fatalf("ordinary log center count=%d dropped=%d", len(center), collector.Dropped())
	}
	warnings, summaries := 0, 0
	for _, record := range center {
		if record.Level != "warn" || !strings.Contains(record.Line, "component=proxy") {
			t.Errorf("ordinary log center record=%+v", record)
		}
		if strings.HasPrefix(record.Line, "repeated warning") {
			warnings++
		}
		if strings.HasPrefix(record.Line, logthrottle.SuppressedMessage) && strings.Contains(record.Line, "suppressed=90") {
			summaries++
		}
	}
	if warnings != 10 || summaries != 1 {
		t.Errorf("ordinary log center warnings=%d summaries=%d", warnings, summaries)
	}
	normal := ordinary.String()
	if got := strings.Count(normal, `level=WARN msg="repeated warning"`); got != 10 {
		t.Errorf("ordinary warnings=%d, want10", got)
	}
	if strings.Contains(normal, "repeated debug") || !strings.Contains(normal, "suppressed=90") || strings.Count(normal, `msg="suppressed similar log messages"`) != 1 || !strings.Contains(normal, "component=proxy") {
		t.Errorf("ordinary log changed: %s", normal)
	}
}
func TestLoggerSessionReturnsToConfiguredLevel(t *testing.T) {
	for _, reason := range []string{"stopped", "deadline", "size_limit"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Unix(1800000000, 0)
			var ordinary strings.Builder
			log, tee, _ := NewLogger(slog.NewTextHandler(&ordinary, &slog.HandlerOptions{Level: slog.LevelInfo}), logthrottle.Options{})
			tee.SetClock(func() time.Time { return now })
			if log.Enabled(context.Background(), slog.LevelDebug) {
				t.Fatal("debug enabled without session")
			}
			limit := int64(1 << 20)
			if reason == "size_limit" {
				limit = 1
			}
			path := filepath.Join(t.TempDir(), "session.log")
			if e := tee.Start("s", path, now.Add(time.Second), limit); e != nil {
				t.Fatal(e)
			}
			if !log.Enabled(context.Background(), slog.LevelDebug) {
				t.Fatal("session did not enable debug")
			}
			log.Debug("inside")
			switch reason {
			case "stopped":
				tee.Stop("")
			case "deadline":
				now = now.Add(time.Second)
			}
			if log.Enabled(context.Background(), slog.LevelDebug) {
				t.Fatal("debug remains enabled after " + reason)
			}
			log.Debug("outside")
			log.Info("ordinary still works")
			st, _ := tee.Status()
			if st.Open || st.Ended != reason {
				t.Fatalf("status=%+v", st)
			}
			if strings.Contains(read(t, path), "outside") || strings.Contains(ordinary.String(), "inside") || strings.Contains(ordinary.String(), "outside") || !strings.Contains(ordinary.String(), "ordinary still works") {
				t.Fatal("session boundary or ordinary level changed")
			}
		})
	}
}
func TestLoggerConcurrentSessionCapturesEveryRecord(t *testing.T) {
	log, tee, _ := NewLogger(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}), logthrottle.Options{})
	path := filepath.Join(t.TempDir(), "session.log")
	if e := tee.Start("s", path, time.Now().Add(time.Hour), 1<<20); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				log.Warn("concurrent warning")
				log.Debug("concurrent debug")
			}
		}()
	}
	wg.Wait()
	tee.Stop("")
	data := read(t, path)
	if strings.Count(data, `level=WARN msg="concurrent warning"`) != 100 || strings.Count(data, `level=DEBUG msg="concurrent debug"`) != 100 {
		t.Fatal("concurrent raw session records missing")
	}
}

func TestLoggerWithGroupPreservesOrdinaryGrouping(t *testing.T) {
	var ordinary strings.Builder
	log, tee, _ := NewLogger(slog.NewTextHandler(&ordinary, nil), logthrottle.Options{})
	path := filepath.Join(t.TempDir(), "session.log")
	if err := tee.Start("s", path, time.Now().Add(time.Hour), 1<<20); err != nil {
		t.Fatal(err)
	}
	log.WithGroup("request").With("component", "proxy").Warn("grouped warning", "code", "CODE")
	tee.Stop("")
	if !strings.Contains(ordinary.String(), "request.component=proxy") || !strings.Contains(ordinary.String(), "request.code=CODE") {
		t.Fatalf("ordinary grouping lost: %s", ordinary.String())
	}
	session := read(t, path)
	if !strings.Contains(session, "component=proxy") || !strings.Contains(session, "code=CODE") {
		t.Fatalf("session attrs lost: %s", session)
	}
}

func TestLoggerKeepsConfiguredDebugForOrdinaryFileOnly(t *testing.T) {
	var ordinary strings.Builder
	now := time.Unix(1800000000, 0)
	collector := selflog.NewCollector(slog.NewTextHandler(&ordinary, &slog.HandlerOptions{Level: slog.LevelDebug}), slog.LevelWarn, 512)
	log, tee, throttle := NewLogger(collector, logthrottle.Options{Now: func() time.Time { return now }})
	tee.SetClock(func() time.Time { return now })
	path := filepath.Join(t.TempDir(), "session.log")
	if err := tee.Start("s", path, now.Add(time.Hour), 1<<20); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		log.Debug("configured debug")
	}
	now = now.Add(31 * time.Second)
	throttle.Sweep(context.Background())
	tee.Stop("")
	if got := strings.Count(read(t, path), `level=DEBUG msg="configured debug"`); got != 100 {
		t.Fatalf("raw debug=%d", got)
	}
	if got := strings.Count(ordinary.String(), `level=DEBUG msg="configured debug"`); got != 50 || !strings.Contains(ordinary.String(), "suppressed=50") {
		t.Fatalf("ordinary debug level/throttle changed: %s", ordinary.String())
	}
	if records := collector.Drain(512); len(records) != 0 {
		t.Fatalf("debug reached warn-only log center: %+v", records)
	}
}
