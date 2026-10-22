package proxy

import (
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const chatPath = "/v1/chat/completions"

func sseHeader() http.Header {
	return http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}}
}

func jsonHeader() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

// feed hands body to the observer in pieces of random size, as reads off a
// tunnel arrive, advancing the observer's clock by one millisecond per piece.
func feed(t *testing.T, o *responseObserver, body string, seed int64) {
	t.Helper()
	clock := o.started
	o.now = func() time.Time { return clock }
	rng := rand.New(rand.NewSource(seed))
	for len(body) > 0 {
		n := min(1+rng.Intn(700), len(body))
		clock = clock.Add(time.Millisecond)
		o.observe([]byte(body[:n]))
		body = body[n:]
	}
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Real replies captured from the test environment: the first token is the
// first non-empty text, never the role-only or empty-content opener, and the
// usage is the engine's, read from the last event however the reads split it.
func TestObserverReadsRealStreams(t *testing.T) {
	for _, tc := range []struct {
		fixture                    string
		prompt, completion, cached uint64
	}{
		// TensorFold puts its own stats object beside usage in the last event.
		{"tensorfold-gb10-stream.sse", 17, 40, 0},
		// Strata opens with an empty content, sends keep-alive comments, starts
		// with reasoning_content and reports llama.cpp timings after usage.
		{"strata-stream.sse", 64, 80, 59},
	} {
		body := readFixture(t, tc.fixture)
		for seed := int64(1); seed <= 20; seed++ {
			o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(100, 0))
			if o == nil {
				t.Fatal("no observer for a chat stream")
			}
			feed(t, o, body, seed)
			stats := o.stats(true)
			if !stats.observed || !stats.usage || stats.prompt != tc.prompt || stats.completion != tc.completion || stats.cached != tc.cached {
				t.Fatalf("%s seed %d: stats = %+v", tc.fixture, seed, stats)
			}
			if !stats.hasFirstToken || stats.firstToken <= 0 {
				t.Fatalf("%s seed %d: no first token: %+v", tc.fixture, seed, stats)
			}
			// The first token arrives within the opening events: well before
			// the clock has run through every piece of the stream.
			if total := time.Duration(strings.Count(body, "")) * time.Millisecond; stats.firstToken >= total {
				t.Fatalf("%s seed %d: first token at %v", tc.fixture, seed, stats.firstToken)
			}
		}
	}
}

func chunk(delta string) string {
	return `data: {"choices":[{"index":0,"delta":` + delta + `,"finish_reason":null}],"usage":null}` + "\n\n"
}

// The first token is timed at the read that carries it.
func TestObserverTimesTheFirstNonEmptyText(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		at     int // index of the event that carries the first token; -1 none
	}{
		{"role then content", []string{chunk(`{"role":"assistant"}`), chunk(`{"content":""}`), chunk(`{"content":"Hi"}`)}, 2},
		{"reasoning first", []string{chunk(`{"role":"assistant","content":""}`), chunk(`{"reasoning":"think"}`)}, 1},
		{"tool call first", []string{chunk(`{"role":"assistant"}`), chunk(`{"tool_calls":[{"index":0,"function":{"name":"f"}}]}`)}, 1},
		{"null is not text, a space is", []string{chunk(`{"content":null}`), chunk(`{"content":" "}`)}, 1},
		{"an empty tool call list is not text", []string{chunk(`{"tool_calls":[]}`), chunk(`{"content":"x"}`)}, 1},
		{"no text at all", []string{chunk(`{"role":"assistant"}`), "data: [DONE]\n\n"}, -1},
		{"CRLF and a comment", []string{": ping\r\n\r\n", strings.ReplaceAll(chunk(`{"content":"x"}`), "\n", "\r\n")}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(100, 0)
			clock := start
			o := newResponseObserver(chatPath, 200, sseHeader(), start)
			o.now = func() time.Time { return clock }
			for i, event := range tc.events {
				clock = start.Add(time.Duration(i+1) * time.Second)
				// Split each event across two reads inside its JSON.
				half := len(event) / 2
				o.observe([]byte(event[:half]))
				o.observe([]byte(event[half:]))
			}
			stats := o.stats(true)
			if tc.at < 0 {
				if stats.hasFirstToken {
					t.Fatalf("first token found: %+v", stats)
				}
				return
			}
			if !stats.hasFirstToken || stats.firstToken != time.Duration(tc.at+1)*time.Second {
				t.Fatalf("first token = %+v, want at %ds", stats, tc.at+1)
			}
		})
	}
}

// A long reply pushes its start out of the tail: the usage at the end is still
// read whole, through the ring's wrap, and a null usage repeated in the
// chunks before it does not hide it.
func TestObserverReadsUsageAtTheEndOfALongReply(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 400; i++ {
		body.WriteString(chunk(fmt.Sprintf(`{"content":"token %d with \"usage\": {\"prompt_tokens\": 999}"}`, i)))
	}
	body.WriteString(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":400,"prompt_tokens_details":{"cached_tokens":1024}}}` + "\n\n")
	body.WriteString(chunk(`{}`)) // a trailing chunk with "usage": null
	body.WriteString("data: [DONE]\n\n")
	if body.Len() < 4*usageTailBytes {
		t.Fatalf("reply of %d bytes does not wrap the tail", body.Len())
	}
	for seed := int64(1); seed <= 30; seed++ {
		o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
		feed(t, o, body.String(), seed)
		stats := o.stats(true)
		if !stats.usage || stats.prompt != 1200 || stats.completion != 400 || stats.cached != 1024 {
			t.Fatalf("seed %d: stats = %+v", seed, stats)
		}
	}
	// One read larger than the tail.
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	o.observe([]byte(body.String()))
	if stats := o.stats(true); stats.prompt != 1200 {
		t.Fatalf("single read: %+v", stats)
	}
}

func TestObserverReadsPlainJSONReplies(t *testing.T) {
	chat := `{"id":"x","choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`
	o := newResponseObserver(chatPath, 200, jsonHeader(), time.Unix(0, 0))
	o.observe([]byte(chat))
	stats := o.stats(true)
	if !stats.usage || stats.prompt != 9 || stats.completion != 3 || stats.cached != 0 || stats.hasFirstToken {
		t.Fatalf("chat = %+v", stats)
	}
	// An embeddings reply: a large vector body, the usage after it; it has no
	// completion tokens to report.
	embeddings := `{"data":[{"embedding":[` + strings.Repeat("0.123456,", 3000) + `0.1]}],"usage":{"prompt_tokens":7,"total_tokens":7}}`
	o = newResponseObserver("/v1/embeddings", 200, jsonHeader(), time.Unix(0, 0))
	feed(t, o, embeddings, 3)
	if stats := o.stats(true); !stats.usage || stats.prompt != 7 || stats.completion != 0 {
		t.Fatalf("embeddings = %+v", stats)
	}
}

func TestObserverCountsAMissingUsageOnlyForAWholeReply(t *testing.T) {
	stream := chunk(`{"content":"a"}`) + "data: [DONE]\n\n"
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	o.observe([]byte(stream))
	if stats := o.stats(true); !stats.observed || stats.usage || !stats.hasFirstToken {
		t.Fatalf("whole reply without usage = %+v", stats)
	}
	// A truncated or abandoned reply is neither: its usage may simply not
	// have arrived yet. Its first token still counts.
	o = newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	o.observe([]byte(stream))
	if stats := o.stats(false); stats.observed || stats.usage || !stats.hasFirstToken {
		t.Fatalf("truncated reply = %+v", stats)
	}
	// A usage cut off at the tail's end is not a usage.
	o = newResponseObserver(chatPath, 200, jsonHeader(), time.Unix(0, 0))
	o.observe([]byte(`{"usage":{"prompt_tokens":5,"completion_tokens":`))
	if stats := o.stats(true); stats.usage {
		t.Fatalf("partial usage = %+v", stats)
	}
}

func TestObserverSkipsRepliesItCannotRead(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		status int
		header http.Header
	}{
		{"operation without usage", "/v1/audio/speech", 200, jsonHeader()},
		{"unknown path", "/v1/whatever", 200, jsonHeader()},
		{"error status", chatPath, 500, jsonHeader()},
		{"compressed", chatPath, 200, http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"gzip"}}},
		{"binary body", chatPath, 200, http.Header{"Content-Type": {"application/octet-stream"}}},
	}
	for _, tc := range cases {
		if o := newResponseObserver(tc.path, tc.status, tc.header, time.Unix(0, 0)); o != nil {
			t.Fatalf("%s: observer created", tc.name)
		}
	}
	if o := newResponseObserver(chatPath, 200, http.Header{"Content-Type": {"text/event-stream"}, "Content-Encoding": {"identity"}}, time.Unix(0, 0)); o == nil {
		t.Fatal("identity encoding refused")
	}
	// A nil observer is inert.
	var none *responseObserver
	none.observe([]byte("x"))
	if stats := none.stats(true); stats.observed {
		t.Fatalf("nil observer stats = %+v", stats)
	}
}

// The whole stream is timed, however long the reasoning before the answer:
// a first answer far into the stream is still found. A line longer than any
// text event is skipped without being buffered, and its stream's usage is
// still read.
func TestObserverTimesTheWholeStreamAndSkipsEndlessLines(t *testing.T) {
	filler := ": " + strings.Repeat("x", 1000) + "\n"
	body := strings.Repeat(filler, 300) + chunk(`{"content":"late"}`) +
		`data: {"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1}}` + "\n\n"
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	feed(t, o, body, 5)
	if stats := o.stats(true); !stats.hasFirstToken || !stats.hasAnswer || stats.prompt != 2 {
		t.Fatalf("late first token = %+v", stats)
	}
	o = newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	o.observe([]byte("data: " + strings.Repeat("y", sseLineMax+10)))
	if !o.skipLine || len(o.line) != 0 {
		t.Fatal("an endless line was buffered")
	}
	o.observe([]byte("yyy\n" + chunk(`{"content":"after"}`)))
	if stats := o.stats(true); !stats.hasAnswer || stats.events != 1 {
		t.Fatalf("the line after a skipped one = %+v", stats)
	}
}

// BenchmarkObserveStream is the gateway's added cost for one streamed reply:
// the real Strata reply (81 events, 18 KiB) in 64 reads, its first token found
// and its usage parsed.
func BenchmarkObserveStream(b *testing.B) {
	data, err := os.ReadFile("testdata/strata-stream.sse")
	if err != nil {
		b.Fatal(err)
	}
	reads := make([][]byte, 0, 64)
	for step, rest := len(data)/64+1, data; len(rest) > 0; {
		n := min(step, len(rest))
		reads = append(reads, rest[:n])
		rest = rest[n:]
	}
	header := sseHeader()
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		o := newResponseObserver(chatPath, 200, header, time.Unix(0, 0))
		for _, read := range reads {
			o.observe(read)
		}
		if stats := o.stats(true); !stats.usage {
			b.Fatal("no usage")
		}
	}
}

// A client that closes as soon as it has a reply's last event leaves only
// the stream's terminator unread: the reply is whole and its usage counts.
// A cut before the last event, or a running usage count before the finish
// reason (continuous usage stats), is not the reply's usage.
func TestObserverTakesTheUsageOfAReplyWhoseLastEventArrived(t *testing.T) {
	usage := `"usage":{"prompt_tokens":15,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":0}}`
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]`
	for _, tc := range []struct {
		name  string
		body  string
		ended bool
	}{
		// TensorFold, Strata: usage in the event that finishes.
		{"usage in the last event", chunk(`{"content":"a"}`) + "data: " + finish + `,"tensorfold":{"prefill_s":0.07},` + usage + "}\n\n", true},
		// vLLM, SGLang asked for usage: it follows in its own event.
		{"usage after the last event", chunk(`{"content":"a"}`) + "data: " + finish + "}\n\n" + `data: {"choices":[],` + usage + "}\n\n", true},
		{"cut before the usage event", chunk(`{"content":"a"}`) + "data: " + finish + "}\n\n", false},
		{"running usage before the finish", `data: {"choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null}],` + usage + "}\n\n", false},
	} {
		o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
		o.observe([]byte(tc.body))
		stats := o.stats(false)
		if stats.ended != tc.ended || stats.usage != tc.ended || stats.observed != tc.ended {
			t.Errorf("%s: ended %v usage %v observed %v, want %v", tc.name, stats.ended, stats.usage, stats.observed, tc.ended)
		}
		if tc.ended && (stats.prompt != 15 || stats.completion != 40 || !stats.cachedKnown) {
			t.Errorf("%s: usage = %+v", tc.name, stats)
		}
	}
}
