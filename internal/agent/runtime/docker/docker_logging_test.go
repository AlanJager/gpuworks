package docker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// captureInfoLogs swaps the default logger for a buffer that records INFO and
// above, mirroring how the production agent configures its file log.
func captureInfoLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// Mutating commands are the audit trail for "who touched this node": they must
// appear at INFO even when they succeed, while pure reads stay below INFO. A
// stop that only surfaces in Debug is invisible in production agent logs — the
// exact blind spot behind the gpu3 orphan-sweep incident.
func TestMutatingCommandsLogAtInfoAndReadsStayQuiet(t *testing.T) {
	// inspect answers like a running container; every other command succeeds.
	installFakeDocker(t, "case \"$1\" in inspect) echo 'true|0|false|';; esac; exit 0")
	d := New()

	logs := captureInfoLogs(t)
	if err := d.Stop(context.Background(), "ctr"); err != nil {
		t.Fatal(err)
	}
	got := logs.String()
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "stop") || !strings.Contains(got, "ctr") {
		t.Fatalf("expected INFO log of the stop command, got: %q", got)
	}

	logs.Reset()
	if err := d.Remove(context.Background(), "ctr"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "[rm -f ctr]") {
		t.Fatalf("expected INFO log of the rm command, got: %q", logs.String())
	}

	logs.Reset()
	if _, err := d.Status(context.Background(), "ctr"); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Fatalf("pure read leaked into INFO logs: %q", logs.String())
	}
}
