package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// The gateway's own measurements of a forwarded reply (ADR 0039, ADR 0042):
// when a stream's first reasoning and first answer text arrived, how many
// text events it carried and the longest pause between them, and what the
// engine wrote into the reply's end - its usage, the finish reason and any
// per-request stats the deployment's engine or template declares. All of it
// comes from the bytes the gateway copies anyway; the observer never
// changes, delays or buffers what the client receives.
//
// Usage and stats are read from the end of the reply only: an engine reports
// them in the last event of a stream or alongside a plain reply's result, so
// a small ring of the last bytes holds them and nothing else is parsed. Tokens
// are never counted from events: one event may carry several tokens
// (TensorFold's DFlash2 sends 40 tokens in 8), and only the engine's
// tokenizer knows the count. Events are counted to time the stream.
const (
	// usageTailBytes is the reply tail kept for usage. A stream's usage event
	// is well under 1 KiB (artifacts/work/2026-10-10-inference-metrics-sources/evidence).
	usageTailBytes = 4 << 10
	// sseLineMax bounds one buffered SSE line. A longer line is skipped: no
	// text event that times a stream is that long.
	sseLineMax = 64 << 10
)

// tailPool holds mirrored rings: every byte is written at i and at
// i+usageTailBytes, so the last usageTailBytes bytes are always one
// contiguous slice and reading the tail never has to reorder the ring.
var tailPool = sync.Pool{New: func() any { return new([2 * usageTailBytes]byte) }}

// responseStats is what one reply contributed. Durations are from the
// moment the request was handed to the instance.
type responseStats struct {
	// observed: the reply was one whose usage the gateway reads (a complete
	// 2xx JSON or SSE reply of an operation that reports usage).
	observed bool
	// usage: the reply carried usage; the token counts are valid.
	usage                      bool
	prompt, completion, cached uint64
	// cachedKnown: the usage stated a cache figure (0 included).
	cachedKnown bool
	// reasoning is the completion tokens spent reasoning; reasoningKnown
	// whether the usage said.
	reasoning      uint64
	reasoningKnown bool
	// firstToken is the time to the first generated token of a stream.
	firstToken    time.Duration
	hasFirstToken bool
	// stream timeline: first reasoning text, first answer text (content or a
	// tool call), last text event; the has* flags say which happened.
	firstThink, firstAnswer, lastEvent time.Duration
	hasThink, hasAnswer                bool
	events, thinkEvents                int
	maxGap                             time.Duration
	finishReason                       string
	// ended: the reply's last event (finish reason and usage) arrived, so
	// the reply is whole even if the copy stopped before its end.
	ended bool
	// engine is what the engine reported about the request, by request stat
	// name, in seconds.
	engine map[string]float64
}

// replyReading is how a deployment's replies are read beyond the
// operation's usage shape.
type replyReading struct {
	// rules are the request stats (inferenceapi.ResolveRequestStats).
	rules []inferencemetrics.RequestStatRule
	// cacheOmitsZero: the engine reports its cache hits but leaves the
	// figure out when there were none (SGLang with --enable-cache-report
	// writes prompt_tokens_details only for a count above zero), so usage
	// without it means nothing was cached.
	cacheOmitsZero bool
	// fallbacks are the gateway histograms recorded for this deployment in
	// place of engine histograms its engine does not export
	// (inferencemetrics.GatewayFallbacksFor).
	fallbacks []inferencemetrics.GatewayFallback
}

// responseObserver watches one reply as the reverse proxy copies it. Read and
// Close of the upstream body - and so observe and stats - run on the copying
// goroutine only, so it needs no lock; live is the one thing other goroutines
// read, through its atomics.
type responseObserver struct {
	shape   inferenceapi.Usage
	stream  bool
	started time.Time
	now     func() time.Time
	// reading is how the deployment's replies are read.
	reading replyReading
	// live mirrors the timeline for the in-flight view; nil when unregistered.
	live *liveRequest

	tail    *[2 * usageTailBytes]byte
	tailPos int // next write position in the ring, in [0, usageTailBytes)
	tailLen int // valid bytes in the ring

	line     []byte
	skipLine bool // the current line outgrew sseLineMax
	// readAt is when the bytes being scanned arrived, taken once per read:
	// the events of one read arrived together. Zero until the read's first
	// event needs it.
	readAt   time.Duration
	timeline responseStats
	// keys are the quoted stream text keys, built once; thinkKey marks the
	// reasoning ones.
	keys     [][]byte
	thinkKey []bool
	// inChoice: the stream text keys are members of the choice itself (text
	// completion), not of its delta.
	inChoice bool
}

// newResponseObserver observes a reply of the operation at path, or returns
// nil when there is nothing to observe: the operation reports no usage, the
// reply failed, or its body is compressed or neither JSON nor SSE.
func newResponseObserver(path string, status int, header map[string][]string, started time.Time) *responseObserver {
	shape, ok := inferenceapi.UsageForPath(path)
	if !ok || status < 200 || status >= 300 {
		return nil
	}
	if encoding := strings.TrimSpace(first(header["Content-Encoding"])); encoding != "" && !strings.EqualFold(encoding, "identity") {
		return nil
	}
	contentType := strings.ToLower(first(header["Content-Type"]))
	stream := strings.HasPrefix(contentType, "text/event-stream")
	if !stream && !strings.Contains(contentType, "json") {
		return nil
	}
	o := &responseObserver{
		shape: shape, stream: stream && len(shape.StreamText) > 0, started: started, now: time.Now,
		tail: tailPool.Get().(*[2 * usageTailBytes]byte),
	}
	if o.stream {
		o.keys, o.thinkKey = streamKeys(shape.StreamText)
		o.inChoice = shape.StreamTextInChoice
	}
	return o
}

// streamKeyCache holds each operation's quoted stream keys, so an observer
// builds none per request.
var streamKeyCache sync.Map // strings.Join(StreamText, ",") -> streamKeySet

type streamKeySet struct {
	keys  [][]byte
	think []bool
}

func streamKeys(text []string) ([][]byte, []bool) {
	id := strings.Join(text, ",")
	if cached, ok := streamKeyCache.Load(id); ok {
		set := cached.(streamKeySet)
		return set.keys, set.think
	}
	set := streamKeySet{}
	for _, key := range text {
		set.keys = append(set.keys, []byte(`"`+key+`"`))
		set.think = append(set.think, reasoningKeys[key])
	}
	streamKeyCache.Store(id, set)
	return set.keys, set.think
}

// deltaWindow bounds where a text key is looked for after "delta": engines
// put the text first in the delta, and only the value's first bytes matter.
const deltaWindow = 512

// deltaSearch bounds where the "delta" key itself is looked for in a chunk.
const deltaSearch = 2 << 10

var deltaKey = []byte(`"delta"`)

// choicesKey anchors the search for text that a choice carries directly.
var choicesKey = []byte(`"choices"`)

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// observe takes the next bytes read from the upstream body.
func (o *responseObserver) observe(p []byte) {
	if o == nil || len(p) == 0 || o.tail == nil {
		return
	}
	if o.stream {
		o.readAt = 0
		o.scan(p)
	}
	o.keepTail(p)
}

func (o *responseObserver) keepTail(p []byte) {
	if len(p) > usageTailBytes {
		p = p[len(p)-usageTailBytes:]
	}
	ring := o.tail[:usageTailBytes]
	n := copy(ring[o.tailPos:], p)
	copy(ring, p[n:])
	// The mirror half: the same bytes usageTailBytes further on.
	mirror := o.tail[usageTailBytes:]
	copy(mirror[o.tailPos:], p[:n])
	copy(mirror, p[n:])
	o.tailPos = (o.tailPos + len(p)) % usageTailBytes
	o.tailLen = min(o.tailLen+len(p), usageTailBytes)
}

// scan splits a stream into lines and times each text event.
func (o *responseObserver) scan(p []byte) {
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			if !o.skipLine {
				o.line = append(o.line, p...)
				if len(o.line) > sseLineMax {
					o.line, o.skipLine = o.line[:0], true
				}
			}
			return
		}
		line := p[:end]
		p = p[end+1:]
		if o.skipLine {
			o.skipLine = false
			continue
		}
		if len(o.line) > 0 {
			line = append(o.line, line...)
			o.line = o.line[:0]
		}
		o.event(line)
	}
}

// reasoningKeys are the stream text keys that carry reasoning; the others in
// the operation's StreamText are the answer.
var reasoningKeys = map[string]bool{"reasoning_content": true, "reasoning": true}

// event classifies one SSE line: a data event carrying non-empty reasoning
// text, answer text, both, or neither (role-only openers, finish and usage
// events, keep-alive comments).
func (o *responseObserver) event(line []byte) {
	payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok {
		return
	}
	// Engines put the delta near the start of a chunk, after its id, model
	// and choice index; a line without one there carries no stream text. A
	// text completion has no delta: its first choice holds the text.
	anchor := deltaKey
	if o.inChoice {
		anchor = choicesKey
	}
	at := bytes.Index(payload[:min(len(payload), deltaSearch)], anchor)
	if at < 0 {
		return
	}
	window := payload[at+len(anchor) : min(len(payload), at+deltaWindow)]
	var think, answer bool
	if o.inChoice {
		think, answer = o.choiceText(window)
	} else {
		think, answer = o.deltaText(window)
	}
	if !think && !answer {
		return
	}
	if o.readAt == 0 {
		o.readAt = max(o.now().Sub(o.started), 1)
	}
	readAt := o.readAt
	t := &o.timeline
	if t.events > 0 && readAt-t.lastEvent > t.maxGap {
		t.maxGap = readAt - t.lastEvent
	}
	t.events++
	t.lastEvent = readAt
	if !t.hasFirstToken {
		t.firstToken, t.hasFirstToken = readAt, true
	}
	if think {
		t.thinkEvents++
		if !t.hasThink {
			t.firstThink, t.hasThink = readAt, true
		}
	}
	if answer && !t.hasAnswer {
		t.firstAnswer, t.hasAnswer = readAt, true
	}
	o.live.noteEvent(readAt, think, answer)
}

// choiceText reads the first choice of a "choices" array (b starts right
// after the "choices" key) as deltaText reads a delta.
func (o *responseObserver) choiceText(b []byte) (think, answer bool) {
	b = bytes.TrimLeft(b, " \t")
	if len(b) == 0 || b[0] != ':' {
		return false, false
	}
	b = bytes.TrimLeft(b[1:], " \t")
	if len(b) == 0 || b[0] != '[' {
		return false, false
	}
	return o.objectText(bytes.TrimLeft(b[1:], " \t"))
}

// deltaText reads the members of a delta object (b starts right after the
// "delta" key) in order and reports whether a reasoning key and an answer key
// carry non-empty text. It looks at member names and the first bytes of
// values only, and stops at the first value it cannot skip cheaply (an array
// or object - after checking whether it is a non-empty tool call list).
func (o *responseObserver) deltaText(b []byte) (think, answer bool) {
	b = bytes.TrimLeft(b, " \t")
	if len(b) == 0 || b[0] != ':' {
		return false, false
	}
	return o.objectText(bytes.TrimLeft(b[1:], " \t"))
}

// objectText reads the members of the object b starts with and reports
// whether a reasoning key and an answer key carry non-empty text.
func (o *responseObserver) objectText(b []byte) (think, answer bool) {
	if len(b) == 0 || b[0] != '{' {
		return false, false
	}
	b = b[1:]
	for members := 0; members < 8; members++ {
		b = bytes.TrimLeft(b, " \t,")
		if len(b) < 2 || b[0] != '"' {
			return think, answer
		}
		end := bytes.IndexByte(b[1:], '"')
		if end < 0 {
			return think, answer
		}
		name := b[:end+2]
		value := bytes.TrimLeft(b[end+2:], " \t")
		if len(value) == 0 || value[0] != ':' {
			return think, answer
		}
		value = bytes.TrimLeft(value[1:], " \t")
		known := -1
		for i, key := range o.keys {
			if bytes.Equal(name, key) {
				known = i
				break
			}
		}
		if len(value) < 2 {
			return think, answer
		}
		switch value[0] {
		case '"':
			if known >= 0 && value[1] != '"' {
				if o.thinkKey[known] {
					think = true
				} else {
					answer = true
				}
			}
			rest, ok := skipJSONString(value)
			if !ok {
				return think, answer
			}
			b = rest
		case '[':
			if known >= 0 {
				inner := bytes.TrimLeft(value[1:], " \t")
				if len(inner) > 0 && inner[0] != ']' {
					answer = true
				}
			}
			return think, answer
		default:
			// null, a number or an object: skip a short scalar, stop at an
			// object.
			if value[0] == '{' {
				return think, answer
			}
			next := bytes.IndexAny(value, ",}")
			if next < 0 || value[next] == '}' {
				return think, answer
			}
			b = value[next:]
		}
	}
	return think, answer
}

// skipJSONString returns what follows the JSON string b starts with.
func skipJSONString(b []byte) ([]byte, bool) {
	for i := 1; i < len(b); {
		next := bytes.IndexAny(b[i:], `"\`)
		if next < 0 {
			return nil, false
		}
		i += next
		if b[i] == '\\' {
			i += 2
			continue
		}
		return b[i+1:], true
	}
	return nil, false
}

// fieldNonEmpty reports whether payload has key with a non-empty string or
// array value. It reads bytes only: a key inside a JSON string is escaped
// (\"key\") and so never matches the quoted needle.
func fieldNonEmpty(payload []byte, key string) bool {
	return keyNonEmpty(payload, []byte(`"`+key+`"`))
}

// keyNonEmpty is fieldNonEmpty for an already quoted key.
func keyNonEmpty(payload, needle []byte) bool {
	for rest := payload; ; {
		at := bytes.Index(rest, needle)
		if at < 0 {
			return false
		}
		rest = rest[at+len(needle):]
		value := bytes.TrimLeft(rest, " \t")
		if len(value) == 0 || value[0] != ':' {
			continue
		}
		value = bytes.TrimLeft(value[1:], " \t")
		if len(value) < 2 {
			return false
		}
		switch value[0] {
		case '"':
			return value[1] != '"'
		case '[':
			inner := bytes.TrimLeft(value[1:], " \t")
			return len(inner) > 0 && inner[0] != ']'
		}
		return false
	}
}

// stats returns what the reply contributed and gives the tail buffer back.
// clean is whether the reply reached the client whole: a truncated or
// abandoned reply is neither usage nor a missing usage, though its timeline
// stands.
func (o *responseObserver) stats(clean bool) responseStats {
	if o == nil || o.tail == nil {
		return responseStats{}
	}
	out := o.timeline
	tail := o.linearTail()
	finishReason, finishAt := lastStringFieldAt(tail, "finish_reason")
	out.finishReason = finishReason
	out.engine = requestStats(tail, o.reading.rules)
	usage, usageAt, hasUsage := lastObjectAt(tail, "usage")
	// A reply whose last event has arrived - a finish reason, and its usage
	// with it or after it - is whole even if the copy stopped before the
	// stream's terminator: a client may close as soon as it has that event,
	// leaving only "data: [DONE]" unread. A usage before the last finish
	// reason is a running count (continuous usage stats), not the reply's.
	out.ended = finishReason != "" && hasUsage && usageAt > finishAt
	if clean || out.ended {
		out.observed = true
		if hasUsage {
			out.usage = true
			out.prompt, _ = usageCount(usage, []string{o.shape.Prompt})
			if o.shape.Completion != "" {
				out.completion, _ = usageCount(usage, []string{o.shape.Completion})
			}
			out.cached, out.cachedKnown = cacheFigure(usage, o.shape, o.reading)
			for _, path := range o.shape.Reasoning {
				if out.reasoning, out.reasoningKnown = usageCount(usage, path); out.reasoningKnown {
					break
				}
			}
		}
	}
	tailPool.Put(o.tail)
	o.tail = nil
	return out
}

// cacheFigure reads the prompt tokens a reply's usage says the engine's
// cache served: the decode reply's figure here, the prefill instance's in a
// PD handoff (pd_sequential.go).
func cacheFigure(usage map[string]json.RawMessage, shape inferenceapi.Usage, reading replyReading) (uint64, bool) {
	if len(shape.Cached) == 0 {
		return 0, false
	}
	cached, ok := usageCount(usage, shape.Cached)
	if !ok && reading.cacheOmitsZero {
		return 0, true
	}
	return cached, ok
}

// linearTail returns the kept tail in reply order.
func (o *responseObserver) linearTail() []byte {
	if o.tailLen < usageTailBytes {
		return o.tail[:o.tailLen]
	}
	// The oldest kept byte is at tailPos; the mirror makes the next
	// usageTailBytes bytes contiguous.
	return o.tail[o.tailPos : o.tailPos+usageTailBytes]
}
