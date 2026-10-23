package proxy

import (
	"bytes"
	"encoding/json"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Reading the agent-client protocols the gateway forwards beside chat: the
// OpenAI Responses API and the Anthropic Messages API (inferenceapi's
// protocolUsages). Replies captured from vLLM 0.30 and SGLang
// (testdata/*-messages-*, *-responses-*) are what these readers are held to.
//
// Neither protocol keeps its usage where chat does:
//   - a Messages stream reports the prompt in message_start, its first event,
//     and the output in the last message_delta;
//   - a Responses reply echoes the request's tools after its usage (SGLang),
//     so a client with many tools pushes the usage out of the kept tail.
// usageCapture keeps the first and the last usage member wherever they are.

// usageCaptureMax bounds one captured usage member, key included; a usage
// object of these protocols is well under 512 bytes.
const usageCaptureMax = 1 << 10

var usageNeedle = []byte(`"usage"`)

// usageCapture copies the "usage" members of a reply as it streams by: the
// last one, and for a Messages reply the first one (message_start's).
type usageCapture struct {
	first, last []byte
	// before is what preceded the last usage key: a plain Responses reply's
	// status sits right before its usage.
	before    []byte
	keepFirst bool
	filling   bool
	// prev is the end of the previous read: a key split across two reads is
	// still found, and before reaches back into it.
	prev    [usageLookBehind]byte
	prevLen int
}

// usageLookBehind is how much of what precedes a usage key is kept
// ("status":"incomplete", fits).
const usageLookBehind = 48

func (c *usageCapture) observe(p []byte) {
	if len(p) == 0 {
		return
	}
	if c.prevLen > 0 {
		var joined [usageLookBehind + 8]byte
		n := copy(joined[:], c.prev[:c.prevLen])
		n += copy(joined[n:], p[:min(len(p), len(usageNeedle)-1)])
		if at := bytes.Index(joined[:n], usageNeedle); at >= 0 && at < c.prevLen && at+len(usageNeedle) > c.prevLen {
			c.begin(joined[:at], joined[at:c.prevLen])
		}
	}
	if c.filling {
		c.fill(p)
	}
	for off := 0; ; {
		at := bytes.Index(p[off:], usageNeedle)
		if at < 0 {
			break
		}
		at += off
		c.begin(c.lookBehind(p, at), nil)
		c.fill(p[at:])
		off = at + len(usageNeedle)
	}
	c.keepPrev(p)
}

// lookBehind returns up to usageLookBehind bytes before p[at], reaching into
// the previous read.
func (c *usageCapture) lookBehind(p []byte, at int) []byte {
	if at >= usageLookBehind {
		return p[at-usageLookBehind : at]
	}
	var joined [2 * usageLookBehind]byte
	n := copy(joined[:], c.prev[:c.prevLen])
	n += copy(joined[n:], p[:at])
	return joined[max(0, n-usageLookBehind):n]
}

// begin starts a new capture: before precedes the key, prefix is the part
// of the key the previous read held.
func (c *usageCapture) begin(before, prefix []byte) {
	switch {
	case c.last == nil:
		c.last = make([]byte, 0, usageCaptureMax)
	case c.keepFirst && c.first == nil:
		c.first, c.last = c.last, make([]byte, 0, usageCaptureMax)
	default:
		c.last = c.last[:0]
	}
	c.last = append(c.last, prefix...)
	c.before = append(c.before[:0], before[max(0, len(before)-usageLookBehind):]...)
	c.filling = true
}

func (c *usageCapture) fill(p []byte) {
	n := min(usageCaptureMax-len(c.last), len(p))
	c.last = append(c.last, p[:n]...)
	if len(c.last) >= usageCaptureMax {
		c.filling = false
	}
}

func (c *usageCapture) keepPrev(p []byte) {
	if len(p) >= usageLookBehind {
		c.prevLen = copy(c.prev[:], p[len(p)-usageLookBehind:])
		return
	}
	var joined [2 * usageLookBehind]byte
	n := copy(joined[:], c.prev[:c.prevLen])
	n += copy(joined[n:], p)
	c.prevLen = copy(c.prev[:], joined[max(0, n-usageLookBehind):n])
}

// lastUsage is the last captured usage object, if one was an object.
func (c *usageCapture) lastUsage() (map[string]json.RawMessage, bool) {
	return lastObject(c.last, "usage")
}

// firstUsage is the first captured usage object; a reply with one usage has
// it as its last.
func (c *usageCapture) firstUsage() (map[string]json.RawMessage, bool) {
	if c.first == nil {
		return c.lastUsage()
	}
	return lastObject(c.first, "usage")
}

// responsesTypePrefix starts every Responses event type.
const responsesTypePrefix = "response."

var typeKey = []byte(`"type"`)

// responsesEventType returns the type of a Responses event payload without
// its "response." prefix (output_text.delta), or nil. Inner objects carry
// their own types (message, output_text), so the first one naming a response
// event is the event's.
func responsesEventType(payload []byte) []byte {
	for rest := payload; ; {
		at := bytes.Index(rest, typeKey)
		if at < 0 {
			return nil
		}
		rest = rest[at+len(typeKey):]
		value := bytes.TrimLeft(rest, " \t")
		if len(value) == 0 || value[0] != ':' {
			continue
		}
		value = bytes.TrimLeft(value[1:], " \t")
		if len(value) == 0 || value[0] != '"' {
			continue
		}
		value = value[1:]
		end := bytes.IndexByte(value, '"')
		if end < 0 {
			return nil
		}
		if kind, ok := bytes.CutPrefix(value[:end], []byte(responsesTypePrefix)); ok {
			return kind
		}
	}
}

// responsesText classifies a Responses stream event: reasoning text, answer
// text (output text, a refusal, a tool call's arguments), or neither.
func responsesText(payload []byte) (think, answer bool) {
	switch string(responsesEventType(payload)) {
	case "reasoning_text.delta", "reasoning_summary_text.delta":
		return keyNonEmpty(payload, deltaKey), false
	case "output_text.delta", "refusal.delta", "function_call_arguments.delta":
		return false, keyNonEmpty(payload, deltaKey)
	}
	return false, false
}

// responsesFinish reads how a Responses reply ended: the terminal event of a
// stream (response.completed, response.incomplete, response.failed) or the
// status of a plain reply, as a chat finish reason. ended says a stream's
// terminal event arrived.
func responsesFinish(tail []byte, stream bool) (reason string, ended bool) {
	// The response's own status follows its output items' statuses, and it
	// is what counts: vLLM ends a reply cut at max_output_tokens with a
	// response.completed event whose response is "incomplete".
	status := lastStringField(tail, "status")
	if stream {
		for _, terminal := range []string{"completed", "incomplete", "failed"} {
			if bytes.Contains(tail, []byte(`"`+responsesTypePrefix+terminal+`"`)) {
				ended = true
				if status == "" {
					status = terminal
				}
				break
			}
		}
	}
	switch status {
	case "completed":
		return "stop", ended
	case "incomplete":
		if lastStringField(tail, "reason") == "max_output_tokens" {
			return uxrecord.FinishLength, ended
		}
		return "incomplete", ended
	case "failed":
		return "error", ended
	}
	return "", ended
}

// messagesFinish maps a Messages stop_reason to a chat finish reason, with
// the offset of the last stop_reason in the tail.
func messagesFinish(tail []byte) (string, int) {
	reason, at := lastStringFieldAt(tail, "stop_reason")
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop", at
	case "max_tokens":
		return uxrecord.FinishLength, at
	case "tool_use":
		return "tool_calls", at
	}
	return reason, at
}

// messagesUsage adds up a Messages reply's usage. When the cache figures are
// present input_tokens is the uncached part of the prompt (vLLM, Anthropic);
// without them it is the whole prompt (SGLang) and the cache is unknown. The
// last usage (message_delta, or the plain reply's) wins; SGLang leaves the
// input out of it, so message_start's stands in.
func messagesUsage(final, start map[string]json.RawMessage, shape inferenceapi.Usage) (prompt, completion, cached uint64, cachedKnown bool) {
	completion, _ = usageCount(final, []string{shape.Completion})
	source := final
	if _, ok := final[shape.Prompt]; !ok && start != nil {
		source = start
	}
	input, _ := usageCount(source, []string{shape.Prompt})
	read, hasRead := usageCount(source, shape.Cached)
	creation, hasCreation := usageCount(source, []string{"cache_creation_input_tokens"})
	if !hasRead && !hasCreation {
		return input, completion, 0, false
	}
	return input + read + creation, completion, read, hasRead
}

// protocolStats is stats for a Responses or Messages reply.
func (o *responseObserver) protocolStats(out *responseStats, tail []byte, clean bool) {
	usage, usageAt, hasUsage := lastObjectAt(tail, "usage")
	if !hasUsage {
		usage, hasUsage = o.capture.lastUsage()
		usageAt = len(tail)
	}
	if o.shape.Format == inferenceapi.FormatResponses {
		out.finishReason, out.ended = responsesFinish(tail, o.stream)
		if out.finishReason == "" && !o.stream {
			// Echoed tools pushed the status out of the tail with the usage.
			out.finishReason, _ = responsesFinish(o.capture.before, false)
		}
		out.ended = out.ended && hasUsage
	} else {
		var finishAt int
		out.finishReason, finishAt = messagesFinish(tail)
		out.ended = out.finishReason != "" && hasUsage && usageAt > finishAt
	}
	if !clean && !out.ended {
		return
	}
	out.observed = true
	if !hasUsage {
		return
	}
	out.usage = true
	if o.shape.Format == inferenceapi.FormatMessages {
		start, _ := o.capture.firstUsage()
		out.prompt, out.completion, out.cached, out.cachedKnown = messagesUsage(usage, start, o.shape)
		return
	}
	out.prompt, _ = usageCount(usage, []string{o.shape.Prompt})
	out.completion, _ = usageCount(usage, []string{o.shape.Completion})
	out.cached, out.cachedKnown = usageCount(usage, o.shape.Cached)
	for _, path := range o.shape.Reasoning {
		if out.reasoning, out.reasoningKnown = usageCount(usage, path); out.reasoningKnown {
			break
		}
	}
}
