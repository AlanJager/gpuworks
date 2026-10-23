package proxy

import (
	"bufio"
	"bytes"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// The gateway asks the engine for usage on every streamed request it can
// observe, so each request carries its own token counts and, for engines that
// put them on the same event, its own queue/prefill/decode times (ADR 0042).
// It only ever adds `"stream_options":{"include_usage":true}` to a streamed
// request that states no stream_options of its own, and then takes the usage
// event the addition caused back out of the reply (usage_strip.go), so the
// client receives the bytes it would have received without it.

// usageOptionMember is the member inserted as the request object's first.
var usageOptionMember = []byte(`{"include_usage":true}`)

// bodyFacts is what one pass over a request body finds out about it.
type bodyFacts struct {
	// stream is a top-level "stream": true.
	stream bool
	// streamOptions is a top-level "stream_options" member of any value: the
	// client decided about usage itself and the gateway leaves it alone.
	streamOptions bool
	// images counts "image_url" keys at any depth: one per image part of an
	// OpenAI chat message.
	images int32
	bytes  int64
	// sampling are the top-level sampling parameters the request set
	// (max_tokens, max_completion_tokens, temperature, top_p, n).
	sampling uxrecord.Sampling
	// maxCompletion says max_completion_tokens set sampling.MaxTokens, which
	// max_tokens then does not replace.
	maxCompletion bool
}

// samplingKeys are the top-level members whose numeric value the scanner
// keeps.
var samplingKeys = map[string]bool{"max_tokens": true, "max_completion_tokens": true, "temperature": true, "top_p": true, "n": true}

// noteNumber records a sampling member's number; one that does not parse,
// or an integer member that is not a whole number in range, is ignored.
func (f *bodyFacts) noteNumber(key string, raw []byte) {
	v, err := strconv.ParseFloat(string(raw), 64)
	if err != nil {
		return
	}
	whole := v == math.Trunc(v) && v >= 0 && v <= math.MaxInt32
	switch key {
	case "temperature":
		f.sampling.Temperature, f.sampling.HasTemperature = v, true
	case "top_p":
		f.sampling.TopP, f.sampling.HasTopP = v, true
	case "n":
		if whole {
			f.sampling.N, f.sampling.HasN = int32(v), true
		}
	case "max_completion_tokens":
		if whole {
			f.sampling.MaxTokens, f.sampling.HasMaxTokens, f.maxCompletion = int32(v), true, true
		}
	case "max_tokens":
		if whole && !f.maxCompletion {
			f.sampling.MaxTokens, f.sampling.HasMaxTokens = int32(v), true
		}
	}
}

func isNumberByte(c byte) bool {
	return c >= '0' && c <= '9' || c == '-' || c == '+' || c == '.' || c == 'e' || c == 'E'
}

// scanReaders are the body scanner's read buffers: one per request in
// flight would otherwise be allocated for every streamed request.
var scanReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 16<<10) }}

// scanBodyFacts reads a JSON request body once. It tracks the container
// stack and object keys but never decodes values: strings are skipped by
// looking for their closing quote, so a body of base64 images costs little
// more than a copy. A body that is not a JSON object yields zero facts and no
// error.
func scanBodyFacts(source io.Reader) (bodyFacts, error) {
	var facts bodyFacts
	reader := scanReaders.Get().(*bufio.Reader)
	reader.Reset(source)
	defer func() {
		reader.Reset(nil)
		scanReaders.Put(reader)
	}()
	// stack holds the open containers, '{' or '['.
	var stack []byte
	var key []byte
	// expectKey: the next string is an object key. member: the top-level
	// member whose value comes next ("" when none of interest).
	expectKey := false
	member := ""
	inObject := func() bool { return len(stack) > 0 && stack[len(stack)-1] == '{' }
	// num collects the number a sampling member holds; flush records it
	// once a delimiter ends it.
	var num []byte
	flush := func() {
		if len(num) > 0 {
			facts.noteNumber(member, num)
			num = num[:0]
		}
	}
	for {
		c, err := reader.ReadByte()
		if err == io.EOF {
			flush()
			return facts, nil
		}
		if err != nil {
			return facts, err
		}
		facts.bytes++
		switch c {
		case '"':
			s, n, err := readJSONString(reader, expectKey)
			facts.bytes += n
			if err != nil {
				return facts, err
			}
			if !expectKey {
				member = ""
				continue
			}
			key = append(key[:0], s...)
			expectKey = false
			if bytes.Equal(key, []byte("image_url")) {
				facts.images++
			}
			if len(stack) == 1 {
				switch k := string(key); {
				case k == "stream_options":
					facts.streamOptions = true
				case k == "stream", samplingKeys[k]:
					member = k
				}
			}
		case '{':
			stack = append(stack, c)
			expectKey, member = true, ""
		case '[':
			stack = append(stack, c)
			expectKey, member = false, ""
		case '}', ']':
			flush()
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			expectKey, member = false, ""
		case ',':
			flush()
			expectKey, member = inObject(), ""
		case ':':
		case ' ', '\t', '\n', '\r':
			if len(num) > 0 {
				flush()
				member = ""
			}
		case 't':
			if member == "stream" {
				facts.stream = true
			}
			member = ""
		default:
			if samplingKeys[member] && isNumberByte(c) {
				num = append(num, c)
				continue
			}
			num, member = num[:0], ""
		}
	}
}

// readJSONString consumes a string whose opening quote was read. It returns
// the string's raw bytes when keep is set (keys are kept, values only
// skipped) and the number of bytes consumed. A quote ends the string unless
// an odd run of backslashes right before it escapes it; the run may span
// reads, so its length is carried across them.
func readJSONString(reader *bufio.Reader, keep bool) ([]byte, int64, error) {
	var out []byte
	var n int64
	run := 0
	for {
		chunk, err := reader.ReadSlice('"')
		n += int64(len(chunk))
		if err != nil && err != bufio.ErrBufferFull {
			return nil, n, err
		}
		body := chunk
		if err == nil {
			body = chunk[:len(chunk)-1]
		}
		if keep {
			out = append(out, body...)
		}
		trailing := 0
		for i := len(body) - 1; i >= 0 && body[i] == '\\'; i-- {
			trailing++
		}
		if trailing == len(body) {
			run += trailing
		} else {
			run = trailing
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if run%2 == 0 {
			return out, n, nil
		}
		// An escaped quote is part of the string.
		if keep {
			out = append(out, '"')
		}
		run = 0
	}
}

// injectUsage reads a JSON request body's facts and, when inject is set,
// adds stream_options.include_usage to a streamed request of an operation
// that reports usage, and reports whether it did. Every JSON request is read
// (the request view and the trace exporter want its facts whatever the
// engine); a request the gateway cannot read, that does not stream, or that
// states stream_options itself goes out unchanged.
func injectUsage(r *http.Request, maxBody int64, inject bool) (facts bodyFacts, injected bool, cleanup func(), err error) {
	cleanup = func() {}
	if !isJSONRequest(r) || r.GetBody == nil {
		return facts, false, cleanup, nil
	}
	body, err := r.GetBody()
	if err != nil {
		return facts, false, cleanup, err
	}
	facts, err = scanBodyFacts(body)
	_ = body.Close()
	shape, usageOp := inferenceapi.UsageForPath(r.URL.Path)
	if err != nil || !inject || !usageOp || !shape.UsageOnRequest || len(shape.StreamText) == 0 || !facts.stream || facts.streamOptions {
		return facts, false, cleanup, nil
	}
	source, err := r.GetBody()
	if err != nil {
		return facts, false, cleanup, err
	}
	defer source.Close()
	out := newReplayWriter(maxBody)
	if err := insertJSONField(source, out, "stream_options", usageOptionMember); err != nil {
		out.Abort()
		// Not an object after all: forward it as the client sent it.
		return facts, false, cleanup, nil
	}
	replaced, err := out.Close()
	if err != nil {
		return facts, false, cleanup, err
	}
	installReplayBody(r, replaced)
	return facts, true, func() { _ = replaced.Close() }, nil
}
