package proxy

import (
	"bytes"
	"io"
	"math/rand"
	"regexp"
	"strings"
	"testing"
)

// chunkedReader returns its data in pieces of random size, as reads off a
// tunnel arrive.
type chunkedReader struct {
	data   []byte
	rng    *rand.Rand
	closed bool
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(1+r.rng.Intn(900), len(r.data), len(p))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *chunkedReader) Close() error { r.closed = true; return nil }

func strip(t *testing.T, body string, seed int64) (string, *usageStripper, *chunkedReader) {
	t.Helper()
	src := &chunkedReader{data: []byte(body), rng: rand.New(rand.NewSource(seed))}
	s := newUsageStripper(src)
	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil || !src.closed {
		t.Fatalf("close: %v closed=%v", err, src.closed)
	}
	return string(out), s, src
}

var vllmVolatile = regexp.MustCompile(`"id":"(chatcmpl-)?[0-9a-f]+"|"created":[0-9]+`)

// normalize drops what differs between two requests of the same prompt: the
// request id and the creation second.
func normalize(stream string) string { return vllmVolatile.ReplaceAllString(stream, "_") }

// The contract of asking for usage (ADR 0042): what the client receives is
// byte for byte what the engine would have sent had the gateway not asked.
// Each pair below was captured from vLLM 0.30 and SGLang 0.5.21 with the same
// greedy request, once as a client sends it and once with
// stream_options.include_usage added - including vLLM moving
// system_fingerprint onto the usage event.
func TestStripperRestoresTheStreamTheClientAskedFor(t *testing.T) {
	for _, pair := range []struct{ plain, usage string }{
		{"vllm-0.30-plain.sse", "vllm-0.30-usage.sse"},
		{"vllm-0.30-nothink-plain.sse", "vllm-0.30-nothink-usage.sse"},
		{"vllm-0.30-think-plain.sse", "vllm-0.30-think-usage.sse"},
		{"sglang-0.5.21-nothink-plain.sse", "sglang-0.5.21-nothink-usage.sse"},
		{"sglang-0.5.21-think-plain.sse", "sglang-0.5.21-think-usage.sse"},
	} {
		plain, usage := readFixture(t, pair.plain), readFixture(t, pair.usage)
		if normalize(plain) == normalize(usage) {
			t.Fatalf("%s: the captures do not differ; the test proves nothing", pair.usage)
		}
		for seed := int64(1); seed <= 25; seed++ {
			got, s, _ := strip(t, usage, seed)
			if normalize(got) != normalize(plain) {
				t.Fatalf("%s seed %d: stripped stream differs from the plain one\n got tail: %q\nwant tail: %q", pair.usage, seed, tail(got), tail(plain))
			}
			if s.stripped != 1 {
				t.Fatalf("%s seed %d: stripped %d events, want 1", pair.usage, seed, s.stripped)
			}
		}
	}
}

func tail(s string) string {
	if len(s) > 600 {
		return s[len(s)-600:]
	}
	return s
}

// A stream without the usage event - the engine ignored the request, or the
// reply is an error - passes through unchanged, held finish event included.
func TestStripperPassesAStreamWithoutUsageThrough(t *testing.T) {
	plain := readFixture(t, "vllm-0.30-nothink-plain.sse")
	for seed := int64(1); seed <= 10; seed++ {
		if got, s, _ := strip(t, plain, seed); got != plain || s.stripped != 0 {
			t.Fatalf("seed %d: plain stream changed (stripped %d)", seed, s.stripped)
		}
	}
	// A stream cut right after its finish event still delivers it.
	cut := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	if got, _, _ := strip(t, cut, 3); got != cut {
		t.Fatalf("held finish event lost at EOF: %q", got)
	}
	// So does a line the stream never finished.
	partial := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}"
	if got, _, _ := strip(t, partial, 4); got != partial {
		t.Fatalf("partial line lost: %q", got)
	}
}

// SGLang writes "usage": null on every event and may send an extension event
// with no choices; only its real usage event goes.
func TestStripperKeepsSGLangNullUsageAndExtensionEvents(t *testing.T) {
	content := `data: {"id":"a","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}` + "\n\n"
	finish := `data: {"id":"a","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}` + "\n\n"
	ext := `data: {"id":"a","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":null,"sglext":{"cached_tokens_details":{"device":3}}}` + "\n\n"
	usage := `data: {"id":"a","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"total_tokens":7,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":3},"reasoning_tokens":0}}` + "\n\n"
	done := "data: [DONE]\n\n"
	want := content + finish + ext + done
	for seed := int64(1); seed <= 10; seed++ {
		if got, s, _ := strip(t, content+finish+ext+usage+done, seed); got != want || s.stripped != 1 {
			t.Fatalf("seed %d: got %q (stripped %d)", seed, got, s.stripped)
		}
	}
}

// A line longer than any usage event is never held while it arrives, and a
// finish event held before it goes out first.
func TestStripperStreamsLongLinesWithoutHolding(t *testing.T) {
	finish := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n"
	long := "data: " + strings.Repeat("z", usageEventMax*3) + "\n\n"
	src := &chunkedReader{data: []byte(finish + long), rng: rand.New(rand.NewSource(1))}
	s := newUsageStripper(src)
	buf := make([]byte, 64<<10)
	var got bytes.Buffer
	for got.Len() < len(finish)+usageEventMax*2 {
		n, err := s.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("stream ended early: %v", err)
		}
	}
	if !strings.HasPrefix(got.String(), finish+"data: zzz") {
		t.Fatalf("order broken: %q", got.String()[:120])
	}
	rest, _ := io.ReadAll(s)
	if got.String()+string(rest) != finish+long {
		t.Fatal("long line changed")
	}
}

func TestIsUsageOnlyEvent(t *testing.T) {
	for line, want := range map[string]bool{
		`data: {"choices":[],"usage":{"prompt_tokens":1}}`:                        true,
		`data: {"choices": [ ], "usage": {"prompt_tokens":1}}`:                    true,
		`data: {"choices":[],"usage":null}`:                                       false,
		`data: {"choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`:            false,
		`: {"choices":[],"usage":{"prompt_tokens":1}}`:                            false,
		`data: {"choices":[],"text":"\"usage\":{}"}`:                              false,
		`data: {"choices":[{"delta":{"content":"\"choices\":[] \"usage\":{}"}}]}`: false,
	} {
		if got := isUsageOnlyEvent([]byte(line)); got != want {
			t.Errorf("%s: got %v, want %v", line, got, want)
		}
	}
}

// finish_reason is found whether it sits in the event's last bytes or far
// before them (a long field after it), and a null one is not a finish.
func TestFinishesChoice(t *testing.T) {
	long := strings.Repeat("x", finishTail*2)
	for line, want := range map[string]bool{
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`:                           true,
		`data: {"choices":[{"delta":{},"finish_reason":null}]}`:                             false,
		`data: {"choices":[{"delta":{"content":"` + long + `"},"finish_reason":"length"}]}`: true,
		`data: {"choices":[{"delta":{},"finish_reason":"stop","extra":"` + long + `"}]}`:    true,
		`data: {"choices":[{"delta":{},"finish_reason":null,"extra":"` + long + `"}]}`:      false,
		`data: {"choices":[{"delta":{"content":"\"finish_reason\":\"stop\""}}]}`:            false,
		`: {"choices":[{"delta":{},"finish_reason":"stop"}]}`:                               false,
	} {
		if got := finishesChoice([]byte(line)); got != want {
			t.Errorf("%.80s: got %v, want %v", line, got, want)
		}
	}
}

// The event include_usage adds comes after the stream's finish events; a
// usage-shaped event before any choice finished is the engine's own and
// passes through.
func TestStripperOnlyStripsUsageAfterAFinish(t *testing.T) {
	early := `data: {"choices":[],"usage":{"prompt_tokens":3}}` + "\n\n"
	chunk := `data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n"
	finish := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	usage := `data: {"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":1}}` + "\n\n"
	done := "data: [DONE]\n\n"
	for seed := int64(0); seed < 10; seed++ {
		got, s, _ := strip(t, early+chunk+finish+usage+done, seed)
		if got != early+chunk+finish+done || s.stripped != 1 {
			t.Fatalf("seed %d: stripped %d, got %q", seed, s.stripped, got)
		}
	}
}
