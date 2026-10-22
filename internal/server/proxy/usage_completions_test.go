package proxy

import (
	"testing"
	"time"
)

const completionsPath = "/v1/completions"

// What the gateway reads from a real vLLM 0.30 text completion (Qwen3-0.6B,
// --enable-prompt-tokens-details): streamed choices carry their text directly
// with no delta, and the usage arrives in a last chunk with no choices.
func TestObserverReadsVLLMTextCompletionStreams(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		usage   bool
	}{
		{"vllm-0.30-completions-usage.sse", true},
		{"vllm-0.30-completions-plain.sse", false},
	} {
		body := readFixture(t, tc.fixture)
		for seed := int64(1); seed <= 10; seed++ {
			o := newResponseObserver(completionsPath, 200, sseHeader(), time.Unix(100, 0))
			feed(t, o, body, seed)
			stats := o.stats(true)
			if !stats.hasFirstToken || !stats.hasAnswer || stats.hasThink || stats.events != 16 || stats.lastEvent < stats.firstToken {
				t.Fatalf("%s seed %d: timeline %+v", tc.fixture, seed, stats)
			}
			if stats.usage != tc.usage {
				t.Fatalf("%s seed %d: usage read = %v", tc.fixture, seed, stats.usage)
			}
			if tc.usage && (stats.prompt != 5 || stats.completion != 16 || !stats.cachedKnown || stats.cached != 0) {
				t.Fatalf("%s seed %d: usage %+v", tc.fixture, seed, stats)
			}
		}
	}
}

func TestObserverReadsAPlainTextCompletion(t *testing.T) {
	o := newResponseObserver(completionsPath, 200, jsonHeader(), time.Unix(0, 0))
	feed(t, o, readFixture(t, "vllm-0.30-completions.json"), 4)
	if stats := o.stats(true); !stats.usage || stats.prompt != 5 || stats.completion != 16 || stats.hasFirstToken {
		t.Fatalf("completion = %+v", stats)
	}
}

// A chat stream's text is in the delta: the choice-level "text" key of a
// completion must not be looked for there, nor a delta in a completion.
func TestStreamTextIsLookedForWhereTheProtocolPutsIt(t *testing.T) {
	completionLike := "data: " + `{"id":"x","choices":[{"index":0,"text":"Paris"}]}` + "\n\n"
	o := newResponseObserver(chatPath, 200, sseHeader(), time.Unix(0, 0))
	feed(t, o, completionLike, 1)
	if stats := o.stats(true); stats.hasFirstToken {
		t.Fatalf("chat observer took a completion choice as text: %+v", stats)
	}
	chatLike := chunk(`{"content":"Paris"}`)
	o = newResponseObserver(completionsPath, 200, sseHeader(), time.Unix(0, 0))
	feed(t, o, chatLike, 1)
	if stats := o.stats(true); stats.hasFirstToken {
		t.Fatalf("completion observer took a chat delta as text: %+v", stats)
	}
	empty := "data: " + `{"id":"x","choices":[{"index":0,"text":""}]}` + "\n\n" + "data: " + `{"id":"x","choices":[]}` + "\n\n"
	o = newResponseObserver(completionsPath, 200, sseHeader(), time.Unix(0, 0))
	feed(t, o, empty, 1)
	if stats := o.stats(true); stats.hasFirstToken {
		t.Fatalf("empty text counted: %+v", stats)
	}
}
