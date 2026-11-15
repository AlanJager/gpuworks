// body_fields.go reads and edits top-level fields of a replayable JSON
// request body without holding a large body in memory more than once.

package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

func isJSONRequest(r *http.Request) bool {
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		// A media type with a malformed parameter ("application/json; x")
		// is still JSON to an engine that reads only the type; the fields
		// the gateway owns must not slip past it.
		mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

// readerContains reports whether needle occurs in the stream, reading it in
// chunks so a large spooled body is never held in memory at once.
func readerContains(source io.Reader, needle []byte) (bool, error) {
	buf := make([]byte, 64<<10)
	var carry []byte
	for {
		n, err := source.Read(buf)
		if n > 0 {
			window := append(carry, buf[:n]...)
			if bytes.Contains(window, needle) {
				return true, nil
			}
			if keep := len(needle) - 1; len(window) > keep {
				carry = append(carry[:0], window[len(window)-keep:]...)
			} else {
				carry = append(carry[:0], window...)
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

// asciiLetterEscape opens a JSON unicode escape of a character below 0x100;
// hasASCIILetterEscape narrows it to the code points 0x40-0x7f, which hold
// the ASCII letters and the underscore. A client can spell a key the gateway
// owns with such an escape: the engine's parser reads it as the plain key, a
// byte search does not. The escapes clients commonly send (non-ASCII text;
// "<", ">" and "&" from HTML-safe encoders) are outside that range.
var asciiLetterEscape = []byte{'\\', 'u', '0', '0'}

func hasASCIILetterEscape(window []byte) bool {
	for from := 0; ; {
		i := bytes.Index(window[from:], asciiLetterEscape)
		if i < 0 {
			return false
		}
		if at := from + i + len(asciiLetterEscape); at < len(window) && window[at] >= '4' && window[at] <= '7' {
			return true
		}
		from += i + 1
	}
}

// readerContainsEach marks found[i] when needles[i] occurs in the stream,
// in one pass with a pooled buffer, stopping once every needle was seen. A
// stream with an escaped ASCII letter (hasASCIILetterEscape) marks them all,
// so the caller looks its keys up exactly, decoding them.
func readerContainsEach(source io.Reader, needles [][]byte, found []bool) error {
	// keep is how many bytes of a chunk's tail the next one is searched
	// with, so a match across two reads is found; the escape needs its
	// digit after the four bytes.
	keep := len(asciiLetterEscape)
	for _, n := range needles {
		keep = max(keep, len(n)-1)
	}
	bufp := scanBuffers.Get().(*[]byte)
	defer scanBuffers.Put(bufp)
	buf := *bufp
	carry := make([]byte, 0, keep)
	window := make([]byte, 0, len(buf)+keep)
	remaining := len(needles)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			window = append(append(window[:0], carry...), buf[:n]...)
			if hasASCIILetterEscape(window) {
				for i := range found {
					found[i] = true
				}
				return nil
			}
			for i, needle := range needles {
				if !found[i] && bytes.Contains(window, needle) {
					found[i] = true
					remaining--
				}
			}
			if remaining == 0 {
				return nil
			}
			held := min(keep, len(window))
			carry = append(carry[:0], window[len(window)-held:]...)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

var scanBuffers = sync.Pool{New: func() any { b := make([]byte, 64<<10); return &b }}

// setRequestJSONField replaces (or, with insert, adds) a top-level field of
// the request body and installs the result as the new replayable body.
func setRequestJSONField(r *http.Request, key string, encoded []byte, insert bool, maxBody int64) (func(), error) {
	if r.GetBody == nil {
		return func() {}, errors.New("request body is not replayable")
	}
	body, err := r.GetBody()
	if err != nil {
		return func() {}, err
	}
	defer body.Close()
	source, ok := body.(io.ReadSeeker)
	var spooled *replayBody
	if !ok {
		spooled, err = newReplayBodyFromReader(body, maxBody)
		if err != nil {
			return func() {}, err
		}
		defer spooled.Close()
		source = spooled.newReadSeeker()
	}
	out := newReplayWriter(maxBody)
	if err := rewriteJSONField(source, out, key, encoded, insert); err != nil {
		out.Abort()
		return func() {}, err
	}
	replaced, err := out.Close()
	if err != nil {
		return func() {}, err
	}
	installReplayBody(r, replaced)
	return func() { _ = replaced.Close() }, nil
}

// readRequestBody reads the replayable request body, bounded.
func readRequestBody(r *http.Request, maxBody int64) ([]byte, error) {
	if r.GetBody == nil {
		return nil, errors.New("request body is not replayable")
	}
	body, err := r.GetBody()
	if err != nil {
		return nil, err
	}
	defer body.Close()
	limit := maxBody
	if limit <= 0 {
		limit = DefaultMaxBodyBytes
	}
	data, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, tooLarge(limit)
	}
	return data, nil
}

func mustRaw(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
