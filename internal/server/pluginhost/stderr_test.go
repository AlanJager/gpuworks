package pluginhost

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r.Clone())
	return nil
}
func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }

func attr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

// JSON lines keep their level, message and fields under plugin=<id>;
// other lines are info; a flood is cut at the burst and counted; a line
// longer than the limit is cut; the ring keeps the last bytes.
func TestForwardStderr(t *testing.T) {
	var in bytes.Buffer
	in.WriteString(`{"time":"x","level":"ERROR","msg":"store failed","path":"data/db"}` + "\n")
	in.WriteString("plain text line\n")
	in.WriteString(strings.Repeat("x", maxLogLine+100) + "\n")
	for i := 0; i < logLinesBurst+20; i++ {
		in.WriteString("flood\n")
	}
	in.WriteString("no newline at the end")
	h := &captureHandler{}
	keep := newRing(256)
	frozen := time.Unix(1000, 0)
	forwardStderr(&in, "acme", slog.New(h), keep, func() time.Time { return frozen })
	recs := h.records
	if recs[0].Level != slog.LevelError || recs[0].Message != "store failed" || attr(recs[0], "path") != "data/db" || attr(recs[0], "plugin") != "acme" {
		t.Fatalf("json line = %+v", recs[0])
	}
	if recs[1].Level != slog.LevelInfo || recs[1].Message != "plain text line" {
		t.Fatalf("text line = %+v", recs[1])
	}
	if len(recs[2].Message) != maxLogLine {
		t.Fatalf("long line kept %d bytes", len(recs[2].Message))
	}
	last := recs[len(recs)-1]
	if last.Message != "plugin stderr lines dropped over the rate limit" || attr(last, "dropped") != "24" {
		t.Fatalf("last record = %q dropped=%s (of %d records)", last.Message, attr(last, "dropped"), len(recs))
	}
	if got := string(keep.Bytes()); !strings.HasSuffix(got, "flood\nno newline at the end\n") || len(got) != 256 {
		t.Fatalf("ring = %q", got)
	}
}

func TestRing(t *testing.T) {
	r := newRing(8)
	r.Write([]byte("abc"))
	if string(r.Bytes()) != "abc" {
		t.Fatal(string(r.Bytes()))
	}
	r.Write([]byte("defgh"))
	if string(r.Bytes()) != "abcdefgh" {
		t.Fatal(string(r.Bytes()))
	}
	r.Write([]byte("ij"))
	if string(r.Bytes()) != "cdefghij" {
		t.Fatal(string(r.Bytes()))
	}
	r.Write([]byte("0123456789"))
	if string(r.Bytes()) != "23456789" {
		t.Fatal(string(r.Bytes()))
	}
}
