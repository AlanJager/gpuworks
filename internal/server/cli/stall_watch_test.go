package cli

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/stallwatch"
)

func TestStallReportAfterReadsADurationAndZeroTurnsItOff(t *testing.T) {
	for configured, want := range map[string]time.Duration{"3s": 3 * time.Second, "1500ms": 1500 * time.Millisecond, "0": 0, "0s": 0} {
		got, err := stallReportAfter(configured)
		if err != nil || got != want {
			t.Errorf("stallReportAfter(%q) = %v, %v; want %v", configured, got, err, want)
		}
	}
	for _, configured := range []string{"soon", "3", "-1s"} {
		if _, err := stallReportAfter(configured); err == nil || !strings.Contains(err.Error(), "log.stall_report_after") {
			t.Errorf("stallReportAfter(%q) accepted: %v", configured, err)
		}
	}
}

// The server's watch writes into the directory a diagnostics collection
// reads, and a threshold of zero watches nothing.
func TestTheServersStallWatchWritesWhereCollectionsRead(t *testing.T) {
	dataDir := t.TempDir()
	var logged bytes.Buffer
	stop := startStallWatch(slog.New(slog.NewTextHandler(&logged, nil)), 50*time.Millisecond, dataDir)
	end := stallwatch.Begin("database statement")
	deadline := time.Now().Add(10 * time.Second)
	for len(stallwatch.Reports(diagnostics.StallReportDir(dataDir))) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no report under the data directory's diagnostics")
		}
		time.Sleep(10 * time.Millisecond)
	}
	end()
	stop()
	reports := stallwatch.Reports(diagnostics.StallReportDir(dataDir))
	if len(reports) != 1 {
		t.Fatalf("reports: %v, want the one stall", reports)
	}
	raw, err := os.ReadFile(reports[0])
	if err != nil || !strings.Contains(string(raw), "program: gpuworks server ") || !strings.Contains(string(raw), "database statement") {
		t.Fatalf("report: %v\n%s", err, raw)
	}

	off := t.TempDir()
	stop = startStallWatch(slog.New(slog.NewTextHandler(&logged, nil)), 0, off)
	end = stallwatch.Begin("database statement")
	time.Sleep(200 * time.Millisecond)
	end()
	stop()
	if reports := stallwatch.Reports(diagnostics.StallReportDir(off)); len(reports) != 0 {
		t.Fatalf("reports with the watch off: %v", reports)
	}
}

// blockedLog is a log destination that never answers, as a full pipe or a
// disk that stopped does.
type blockedLog struct{ release chan struct{} }

func (b blockedLog) Write(p []byte) (int, error) {
	<-b.release
	return len(p), nil
}

// The server stops its watch on the way out. With the log blocked, the
// watch's reporting goroutine is stuck writing its line; stopping must still
// return.
func TestStoppingTheServersStallWatchDoesNotHangOnABlockedLog(t *testing.T) {
	blocked := blockedLog{release: make(chan struct{})}
	defer close(blocked.release)
	dataDir := t.TempDir()
	stop := startStallWatch(slog.New(slog.NewTextHandler(blocked, nil)), 50*time.Millisecond, dataDir)
	end := stallwatch.Begin("database statement")
	deadline := time.Now().Add(10 * time.Second)
	for len(stallwatch.Reports(diagnostics.StallReportDir(dataDir))) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no report")
		}
		time.Sleep(10 * time.Millisecond)
	}
	end()
	time.Sleep(200 * time.Millisecond) // the end of the stall is now being logged, and waits
	stopped := make(chan struct{})
	go func() { defer close(stopped); stop() }()
	select {
	case <-stopped:
	case <-time.After(stallwatch.DefaultStopWait + 5*time.Second):
		t.Fatal("stopping the watch hung on the blocked log")
	}
}
