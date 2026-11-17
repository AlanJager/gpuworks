package proxy

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestReplyModelRewriterRenamesTheEngineNameOnly(t *testing.T) {
	const engine = "/Users/a/models/q"
	cases := []struct {
		name, in, want string
	}{
		{"json", `{"id":"x","model":"/Users/a/models/q","choices":[]}`, `{"id":"x","model":"qwen","choices":[]}`},
		{"spaced", `{"model" : "/Users/a/models/q", "a":1}`, `{"model" : "qwen", "a":1}`},
		{"escaped", `{"model":"\/Users\/a\/models\/q"}`, `{"model":"qwen"}`},
		{"sse", "data: {\"model\":\"/Users/a/models/q\",\"n\":1}\n\ndata: {\"model\":\"/Users/a/models/q\",\"n\":2}\n\ndata: [DONE]\n\n",
			"data: {\"model\":\"qwen\",\"n\":1}\n\ndata: {\"model\":\"qwen\",\"n\":2}\n\ndata: [DONE]\n\n"},
		{"other name", `{"model":"/Users/a/models/other"}`, `{"model":"/Users/a/models/other"}`},
		{"name as a value", `{"object":"model","owned_by":"/Users/a/models/q"}`, `{"object":"model","owned_by":"/Users/a/models/q"}`},
		{"not a string", `{"model":null,"x":"/Users/a/models/q"}`, `{"model":null,"x":"/Users/a/models/q"}`},
		{"inside text", `{"content":"say \"model\":\"/Users/a/models/q\""}`, `{"content":"say \"model\":\"/Users/a/models/q\""}`},
		{"truncated", `{"model":"/Users/a/mo`, `{"model":"/Users/a/mo`},
		{"truncated key", `{"x":1,"mod`, `{"x":1,"mod`},
		{"empty", ``, ``},
	}
	for _, tc := range cases {
		for _, split := range []struct {
			name string
			wrap func(io.Reader) io.Reader
		}{
			{"whole", func(r io.Reader) io.Reader { return r }},
			{"byte at a time", iotest.OneByteReader},
			{"half", iotest.HalfReader},
		} {
			t.Run(tc.name+"/"+split.name, func(t *testing.T) {
				r := newReplyModelRewriter(io.NopCloser(split.wrap(strings.NewReader(tc.in))), engine, "qwen")
				got, err := io.ReadAll(iotest.OneByteReader(r))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != tc.want {
					t.Fatalf("got %s, want %s", got, tc.want)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// A model value longer than the rewriter holds passes through untouched and
// without being buffered whole.
func TestReplyModelRewriterPassesALongValueThrough(t *testing.T) {
	long := `{"model":"` + strings.Repeat("x", replyModelHoldMax*2) + `","n":1}`
	got, err := io.ReadAll(newReplyModelRewriter(io.NopCloser(iotest.HalfReader(strings.NewReader(long))), "/Users/a/models/q", "qwen"))
	if err != nil || string(got) != long {
		t.Fatalf("long value changed (err %v): %d bytes, want %d", err, len(got), len(long))
	}
}

func TestReplyModelRewriterReportsTheSourceError(t *testing.T) {
	broken := io.MultiReader(strings.NewReader(`{"model":"/Users/a/models/q"`), iotest.ErrReader(io.ErrUnexpectedEOF))
	got, err := io.ReadAll(newReplyModelRewriter(io.NopCloser(broken), "/Users/a/models/q", "qwen"))
	if err != io.ErrUnexpectedEOF || string(got) != `{"model":"qwen"` {
		t.Fatalf("got %q, %v; want the rewritten bytes then the source error", got, err)
	}
}
