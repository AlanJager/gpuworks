package api

import (
	"bytes"
	"testing"
)

func TestWriteSSELogLineUsesPlainTextPayload(t *testing.T) {
	var got bytes.Buffer
	if err := writeSSELogLine(&got, "INFO <ok>\x1b[32mready\x1b[0m"); err != nil {
		t.Fatal(err)
	}
	want := "data: INFO <ok>\x1b[32mready\x1b[0m\n\n"
	if got.String() != want {
		t.Fatalf("SSE payload = %q, want %q", got.String(), want)
	}
}

func TestWriteSSELogLinePrefixesEmbeddedLines(t *testing.T) {
	var got bytes.Buffer
	if err := writeSSELogLine(&got, "first\nsecond\r"); err != nil {
		t.Fatal(err)
	}
	if want := "data: first\ndata: second\n\n"; got.String() != want {
		t.Fatalf("SSE payload = %q, want %q", got.String(), want)
	}
}
