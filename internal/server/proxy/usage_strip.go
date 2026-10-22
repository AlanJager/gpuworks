package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

// usageStripper removes, from a streamed reply the gateway asked usage for
// (usage_inject.go), the one event that asking caused: a data line whose
// choices are empty and whose usage is an object, plus the blank line that
// ends it. Every other byte passes through in order and as soon as its line
// is complete; a line is held back only while it is still arriving, and a
// line longer than usageEventMax is released without being held at all,
// since the usage event is never that long.
//
// One more thing moves when usage is asked for: vLLM puts the stream's
// system_fingerprint on the usage event instead of on the last choice event
// (captured from vLLM 0.30, testdata/vllm-0.30-*.sse). So the event that
// finishes a choice is held, with the blank lines after it, until the next
// event shows whether it is that usage event; if it is, the fingerprint goes
// back where the engine would have put it. The hold lasts from the engine
// writing the finish event to writing the usage event right after it.
type usageStripper struct {
	src io.ReadCloser
	buf []byte
	// pending is a line not yet terminated; out[outPos:] holds bytes ready
	// to return. out is rewound once drained, so its buffer is reused.
	pending []byte
	out     []byte
	outPos  int
	// passthrough: the current line outgrew usageEventMax and is streaming
	// straight through until its newline.
	passthrough bool
	// dropBlank: the usage event was dropped and its terminating blank line
	// goes with it.
	dropBlank bool
	// held is a finish event and the blank lines after it, waiting for the
	// next event.
	held []byte
	// sawFinish: a choice has finished. The usage event comes after the
	// stream's finish events, so lines before the first one are not checked
	// for it.
	sawFinish bool
	err       error
	// stripped counts dropped events; tests read it.
	stripped int
}

// usageEventMax bounds a line held while it arrives: the usage event (and
// vLLM's per-request metrics on it) is well under 2 KiB.
const usageEventMax = 16 << 10

// stripReadBytes is the stripper's read size. An SSE event is far smaller,
// and a stripper lives as long as its stream, so a thousand concurrent
// streams hold 4 MiB rather than the copy buffer's 32 KiB each.
const stripReadBytes = 4 << 10

func newUsageStripper(src io.ReadCloser) *usageStripper {
	return &usageStripper{src: src, buf: make([]byte, stripReadBytes)}
}

func (s *usageStripper) Read(p []byte) (int, error) {
	for s.outPos == len(s.out) {
		s.out, s.outPos = s.out[:0], 0
		if s.err != nil {
			// The stream ended: hand back what was held.
			if len(s.held) > 0 || len(s.pending) > 0 {
				s.out = append(append(s.out, s.held...), s.pending...)
				s.held, s.pending = nil, nil
				break
			}
			return 0, s.err
		}
		n, err := s.src.Read(s.buf)
		s.feed(s.buf[:n])
		if err != nil {
			s.err = err
		}
	}
	n := copy(p, s.out[s.outPos:])
	s.outPos += n
	return n, nil
}

// feed splits new bytes into lines and decides each complete one.
func (s *usageStripper) feed(b []byte) {
	for len(b) > 0 {
		end := bytes.IndexByte(b, '\n')
		if s.passthrough {
			if end < 0 {
				s.out = append(s.out, b...)
				return
			}
			s.out = append(s.out, b[:end+1]...)
			b = b[end+1:]
			s.passthrough = false
			continue
		}
		if end < 0 {
			if len(s.pending)+len(b) > usageEventMax {
				// A long line is never the usage event: whatever was held
				// goes first, in order.
				s.out = append(s.out, s.held...)
				s.held = nil
				s.out = append(s.out, s.pending...)
				s.out = append(s.out, b...)
				s.pending = s.pending[:0]
				s.passthrough = true
				return
			}
			s.pending = append(s.pending, b...)
			return
		}
		line := b[:end+1]
		if len(s.pending) > 0 {
			line = append(s.pending, line...)
			s.pending = s.pending[:0]
		}
		b = b[end+1:]
		s.decide(line)
	}
}

func (s *usageStripper) decide(line []byte) {
	blank := isBlankLine(line)
	if s.dropBlank {
		s.dropBlank = false
		if blank {
			return
		}
	}
	if len(s.held) > 0 {
		if blank {
			s.held = append(s.held, line...)
			return
		}
		held := s.held
		s.held = nil
		if s.sawFinish && isUsageOnlyEvent(line) {
			s.out = append(s.out, withFingerprintOf(held, line)...)
			s.stripped++
			s.dropBlank = true
			return
		}
		s.out = append(s.out, held...)
	}
	if s.sawFinish && isUsageOnlyEvent(line) {
		s.stripped++
		s.dropBlank = true
		return
	}
	if finishesChoice(line) {
		s.sawFinish = true
		s.held = append([]byte(nil), line...)
		return
	}
	s.out = append(s.out, line...)
}

// isBlankLine reports whether a line holds nothing but its line ending.
func isBlankLine(line []byte) bool {
	for _, c := range line {
		if c != '\r' && c != '\n' {
			return false
		}
	}
	return true
}

// finishTail is where a choice event's finish_reason is looked for first:
// engines write it after the delta and logprobs, within the event's last
// hundred-odd bytes.
const finishTail = 256

var finishReasonKey = []byte(`"finish_reason"`)

// finishesChoice reports whether a data line carries a non-null
// finish_reason: the last event of a choice. Every choice event has the key
// (null until the choice ends), so it is looked for in the line's tail
// before the whole line is searched.
func finishesChoice(line []byte) bool {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	if len(line) > finishTail {
		if value, found := finishValue(line[len(line)-finishTail:]); found {
			return value
		}
	}
	value, _ := finishValue(line)
	return value
}

// finishValue looks for the last finish_reason key in b and reports whether
// it holds a string (a finished choice) and whether the key was there. It
// searches forward: bytes.Index is vectorised where bytes.LastIndex is not,
// and a choice event carries the key once.
func finishValue(b []byte) (finished, found bool) {
	for rest := b; ; {
		at := bytes.Index(rest, finishReasonKey)
		if at < 0 {
			return finished, found
		}
		rest = rest[at+len(finishReasonKey):]
		value := bytes.TrimLeft(rest, " \t")
		if len(value) == 0 || value[0] != ':' {
			continue
		}
		value = bytes.TrimLeft(value[1:], " \t")
		finished, found = len(value) > 1 && value[0] == '"' && value[1] != '"', true
	}
}

// withFingerprintOf returns the held finish event (and its blank lines) with
// the usage event's system_fingerprint appended to the finish event's
// top-level object, unless it already has one or the usage event has none.
func withFingerprintOf(held, usage []byte) []byte {
	fingerprint := lastStringField(usage, "system_fingerprint")
	end := bytes.IndexByte(held, '\n')
	if fingerprint == "" || end < 0 || bytes.Contains(held[:end], []byte(`"system_fingerprint"`)) {
		return held
	}
	event := bytes.TrimRight(held[:end], "\r")
	closing := bytes.LastIndexByte(event, '}')
	if closing < 0 {
		return held
	}
	encoded, _ := json.Marshal(fingerprint)
	out := make([]byte, 0, len(held)+len(encoded)+24)
	out = append(out, event[:closing]...)
	out = append(out, `,"system_fingerprint":`...)
	out = append(out, encoded...)
	out = append(out, held[closing:]...)
	return out
}

// isUsageOnlyEvent reports whether an SSE line is a data event with no
// choices that carries usage: the event include_usage adds at the end of a
// stream. A content event never has empty choices.
func isUsageOnlyEvent(line []byte) bool {
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok || !bytes.Contains(payload, []byte(`"usage"`)) {
		return false
	}
	at := bytes.Index(payload, []byte(`"choices"`))
	if at < 0 {
		return false
	}
	rest := bytes.TrimLeft(payload[at+len(`"choices"`):], " \t")
	rest, ok = bytes.CutPrefix(rest, []byte(":"))
	if !ok {
		return false
	}
	rest = bytes.TrimLeft(rest, " \t")
	rest, ok = bytes.CutPrefix(rest, []byte("["))
	if !ok {
		return false
	}
	rest = bytes.TrimLeft(rest, " \t")
	if len(rest) == 0 || rest[0] != ']' {
		return false
	}
	// SGLang writes "usage": null on every event, and an extension event
	// with no choices too; only an object is the usage event.
	usage, ok := lastObject(payload, "usage")
	return ok && usage != nil
}

func (s *usageStripper) Close() error { return s.src.Close() }
