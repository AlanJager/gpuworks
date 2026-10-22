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
	return err == nil && (mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"))
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
