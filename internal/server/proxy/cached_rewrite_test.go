package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// cutReader hands out data in reads of the sizes in cuts, cycling, then
// ends with err (io.EOF when nil).
type cutReader struct {
	data []byte
	cuts []int
	i    int
	err  error
}

func (r *cutReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		return 0, io.EOF
	}
	n := min(len(p), len(r.data))
	if len(r.cuts) > 0 {
		n = min(n, r.cuts[r.i%len(r.cuts)])
		r.i++
	}
	n = copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// rewriteAll runs body through a cachedRewriter, the source cutting its
// reads at cuts and the caller reading into buffers of readSize.
func rewriteAll(t *testing.T, body []byte, cached uint64, cuts []int, readSize int) ([]byte, *cachedRewriter) {
	t.Helper()
	c := newCachedRewriter(io.NopCloser(&cutReader{data: body, cuts: cuts}), "cached_tokens", cached)
	var out bytes.Buffer
	buf := make([]byte, readSize)
	for range 1 << 20 {
		n, err := c.Read(buf)
		out.Write(buf[:n])
		if err == io.EOF {
			return out.Bytes(), c
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	t.Fatal("the rewriter never reached the end")
	return nil, nil
}

var cachedFigure = regexp.MustCompile(`"cached_tokens":\d+`)

// padded is body with every cache figure replaced the way the rewriter
// does it: the new digits, then spaces to the old length.
func padded(body []byte, cached string) []byte {
	return cachedFigure.ReplaceAllFunc(body, func(m []byte) []byte {
		old := len(m) - len(`"cached_tokens":`)
		return []byte(`"cached_tokens":` + cached + strings.Repeat(" ", old-len(cached)))
	})
}

// usageOf decodes the last usage object of a reply, stream or not.
func usageOf(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var last map[string]any
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimPrefix(line, "data: ")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not JSON after the rewrite: %v: %s", err, line)
		}
		if u, ok := event["usage"].(map[string]any); ok {
			last = u
		}
	}
	return last
}

func cachedIn(t *testing.T, body []byte) any {
	t.Helper()
	details, _ := usageOf(t, body)["prompt_tokens_details"].(map[string]any)
	return details["cached_tokens"]
}

// vLLM 0.30's own replies (captured, testdata) - a stream's usage event and
// a plain reply's usage - get the prefill instance's figure in place: the
// same length, every other byte the engine's, still JSON. Whatever way the
// source cuts its reads, and whatever the caller's buffer, the result is
// the same.
func TestCachedRewriterRewritesVLLMRepliesInPlace(t *testing.T) {
	for _, name := range []string{"vllm-0.30-nothink-usage.sse", "vllm-0.30-think-usage.sse", "vllm-0.30-plain.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if cachedIn(t, body) == nil {
				t.Fatal("the capture has no cache figure")
			}
			want := padded(body, "3")
			if bytes.Equal(want, body) || len(want) != len(body) {
				t.Fatal("the capture's figure is already 3")
			}
			for _, c := range []struct {
				cuts     []int
				readSize int
			}{
				{nil, 32 << 10}, {[]int{1}, 32 << 10}, {[]int{7, 3, 50}, 32 << 10}, {[]int{4096}, 32 << 10},
				{nil, 1}, {nil, 10}, {nil, 65}, {[]int{13}, 70},
			} {
				got, rw := rewriteAll(t, body, 3, c.cuts, c.readSize)
				if !bytes.Equal(got, want) {
					t.Fatalf("cuts %v read %d: got\n%s", c.cuts, c.readSize, got)
				}
				if rw.rewritten != 1 {
					t.Fatalf("cuts %v read %d: rewritten %d", c.cuts, c.readSize, rw.rewritten)
				}
			}
			if got := cachedIn(t, want); got != float64(3) {
				t.Fatalf("cached after = %v", got)
			}
		})
	}
}

// Every cut of the figure across two reads: the held tail completes it.
func TestCachedRewriterHoldsAFigureCutAcrossReads(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1036,"prompt_tokens_details":{"cached_tokens" : 1036}},"x":1}`)
	want := []byte(`{"usage":{"prompt_tokens":1036,"prompt_tokens_details":{"cached_tokens" : 512 }},"x":1}`)
	for cut := 1; cut < len(body); cut++ {
		got, _ := rewriteAll(t, body, 512, []int{cut, len(body)}, 32<<10)
		if !bytes.Equal(got, want) {
			t.Fatalf("cut at %d: %s", cut, got)
		}
	}
}

// What is not a figure to rewrite passes through untouched: the key inside
// generated text (its quotes escaped), a key without a number, a figure the
// new one would outgrow, a reply without the key at all, and a key cut at
// the very end of the reply.
func TestCachedRewriterLeavesEverythingElse(t *testing.T) {
	for _, body := range []string{
		`{"choices":[{"delta":{"content":"say \"cached_tokens\":99 please"}}]}`,
		`{"usage":{"cached_tokens":null}}`,
		`{"usage":{"cached_tokens":"7"}}`,
		`{"usage":{"cached_tokens":1.5}}`,
		`{"usage":{"cached_tokens":15e2}}`,
		`{"usage":{"prompt_tokens_details":{"cached_tokens":5}}}`,
		`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\ndata: [DONE]\n\n",
		`{"usage":{"prompt_tokens":4,"x":"cached_tok`,
		`{"usage":{"cached_tokens":12`,
	} {
		got, rw := rewriteAll(t, []byte(body), 880, []int{3}, 32<<10)
		if string(got) != body || rw.rewritten != 0 {
			t.Fatalf("%s -> %s (rewritten %d)", body, got, rw.rewritten)
		}
	}
}

// Continuous usage stats put usage on every event: each gets the figure.
func TestCachedRewriterRewritesEveryUsage(t *testing.T) {
	body := strings.Repeat(`data: {"choices":[{"delta":{"content":"a"}}],"usage":{"prompt_tokens":120,"prompt_tokens_details":{"cached_tokens":120}}}`+"\n\n", 5)
	got, rw := rewriteAll(t, []byte(body), 0, []int{17}, 32<<10)
	if rw.rewritten != 5 || string(got) != string(padded([]byte(body), "0")) {
		t.Fatalf("rewritten %d: %s", rw.rewritten, got)
	}
}

// The source's error comes after every byte, held ones included; Close
// closes the source.
func TestCachedRewriterPassesTheSourceErrorOnAndCloses(t *testing.T) {
	broken := errors.New("tunnel reset")
	closed := false
	body := []byte(`{"usage":{"cached_tokens":1`)
	c := newCachedRewriter(readCloser{Reader: &cutReader{data: body, err: broken}, close: func() error { closed = true; return nil }}, "cached_tokens", 9)
	got, err := io.ReadAll(c)
	if !errors.Is(err, broken) || string(got) != string(body) {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := c.Close(); err != nil || !closed {
		t.Fatalf("close = %v, closed %v", err, closed)
	}
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// The rewriter's own cost over vLLM 0.30's captured replies, against the
// same bytes read straight through:
//
//	go test ./internal/server/proxy -run '^$' -bench 'CachedRewriter' -benchmem -count 10
func BenchmarkCachedRewriter(b *testing.B) {
	for _, name := range []string{"vllm-0.30-think-usage.sse", "vllm-0.30-plain.json"} {
		body, err := os.ReadFile("testdata/" + name)
		if err != nil {
			b.Fatal(err)
		}
		buf := make([]byte, 32<<10)
		for _, rewrite := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/rewrite=%v", name, rewrite), func(b *testing.B) {
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				for b.Loop() {
					var r io.ReadCloser = io.NopCloser(bytes.NewReader(body))
					if rewrite {
						r = newCachedRewriter(r, "cached_tokens", 3)
					}
					for {
						if _, err := r.Read(buf); err != nil {
							break
						}
					}
				}
			})
		}
	}
}

// A whole sequential PD request through the gateway - the prefill call, the
// decode stream with its usage, the record - with the decode reply's cache
// figure rewritten and without (a prefill reply that states none):
//
//	go test ./internal/server/proxy -run '^$' -bench 'PDHandoff' -benchmem -count 10
func BenchmarkPDHandoff(b *testing.B) {
	for _, rewrite := range []bool{false, true} {
		b.Run(fmt.Sprintf("rewrite=%v", rewrite), func(b *testing.B) {
			h := newPDHarness(b, 1, "")
			if !rewrite {
				h.prefills[0].mode = "no_cache_figure"
			}
			h.proxy.SetRequestSink(func(uxrecord.Request) {})
			b.ReportAllocs()
			for b.Loop() {
				if rec := h.do(b, chatPath, pdChat); rec.Code != http.StatusOK {
					b.Fatalf("status %d", rec.Code)
				}
			}
		})
	}
}
