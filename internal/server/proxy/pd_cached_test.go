package proxy

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// pdExchange sends one request through a PD harness and returns what the
// client got and the request's record.
func pdExchange(t *testing.T, h *pdHarness, body string) (string, uxrecord.Request) {
	t.Helper()
	records := make(chan uxrecord.Request, 2)
	h.proxy.SetRequestSink(func(r uxrecord.Request) { records <- r })
	rec := h.do(t, chatPath, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d %s", rec.Code, rec.Body.String())
	}
	select {
	case r := <-records:
		return rec.Body.String(), r
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
		return "", uxrecord.Request{}
	}
}

const pdChatPlain = `{"model":"pd","messages":[{"role":"user","content":"tell me about Rome"}],"max_tokens":64}`

// A handed-off request's decoder counts the pulled KV as cached; the client
// gets the prefill instance's own figure instead - in the stream's usage
// event and in a plain reply, the rest of the usage as the decoder wrote
// it - and the record, the gateway's counters and the exported span read
// the same figure, since the observer reads the rewritten bytes.
func TestAPDReplyReportsThePrefillInstancesCache(t *testing.T) {
	for _, c := range []struct{ name, body string }{{"stream", pdChat}, {"plain", pdChatPlain}} {
		t.Run(c.name, func(t *testing.T) {
			h := newPDHarness(t, 1, "")
			got, r := pdExchange(t, h, c.body)
			if !strings.Contains(got, `"prompt_tokens_details":{"cached_tokens":512 ,"created_cache_tokens":0}`) ||
				!strings.Contains(got, `"prompt_tokens":1036,"total_tokens":1038,"completion_tokens":2`) {
				t.Fatalf("client got %s", got)
			}
			if !r.Usage || r.PromptTokens != 1036 || r.CachedTokens != 512 || r.UncachedTokens() != 524 {
				t.Fatalf("record usage %v prompt %d cached %d", r.Usage, r.PromptTokens, r.CachedTokens)
			}
		})
	}
}

// Where the prefill instance's figure is not the one to report, the
// decoder's stands: a request the decoder computed alone (its own cache), a
// prefill reply without the figure, and the concurrent handoff, where the
// decoder answers before the gateway reads the prefill reply.
func TestAPDReplyKeepsTheDecodersCacheOtherwise(t *testing.T) {
	for _, c := range []struct {
		name, mode, prefill string
		want                int32
	}{
		{"fallback", "vllm-nixl", "500", 0},
		{"no figure", "vllm-nixl", "no_cache_figure", 1036},
		{"concurrent", "sglang-nixl", "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPDHarnessMode(t, c.mode, 1, "")
			h.prefills[0].mode = c.prefill
			got, r := pdExchange(t, h, pdChat)
			if r.CachedTokens != c.want || strings.Contains(got, `"cached_tokens":512`) {
				t.Fatalf("cached %d, client got %s", r.CachedTokens, got)
			}
		})
	}
}

// A repeated prompt: the prefill instance's cache served most of it, a
// figure as long as the decoder's, which fills its digits exactly.
func TestAPDReplyReportsARepeatsCache(t *testing.T) {
	h := newPDHarness(t, 1, "")
	h.prefills[0].cached = 1024
	got, r := pdExchange(t, h, pdChat)
	if r.CachedTokens != 1024 || !strings.Contains(got, `"cached_tokens":1024,`) {
		t.Fatalf("cached %d, client got %s", r.CachedTokens, got)
	}
}
