package nerdctl

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// Mutating commands must reach the production INFO log even on success; pure
// reads stay below INFO (see TestMutatingCommandsLogAtInfoAndReadsStayQuiet in
// the docker package for the incident rationale).
func TestMutatingCommandsLogAtInfoAndReadsStayQuiet(t *testing.T) {
	// inspect answers like a running container; every other command succeeds.
	installFakeNerdctl(t, "case \"$3\" in inspect) echo 'true|0';; esac; exit 0")
	d := New()

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	if err := d.Stop(context.Background(), "ctr"); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if !strings.Contains(got, "level=INFO") || !strings.Contains(got, "stop") || !strings.Contains(got, "ctr") {
		t.Fatalf("expected INFO log of the namespaced stop command, got: %q", got)
	}

	buf.Reset()
	if _, err := d.Status(context.Background(), "ctr"); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Fatalf("pure read leaked into INFO logs: %q", buf.String())
	}
}
