package batch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// verdict is what a reply means for its request.
type verdict int

const (
	// verdictDone: the request has its response; it goes to the output file.
	verdictDone verdict = iota
	// verdictRetry: the platform could not serve it now; it is sent again
	// after a backoff, until the batch expires. It does not count as failed.
	verdictRetry
	// verdictFail: the request cannot succeed as sent; it goes to the error
	// file and counts as failed.
	verdictFail
)

// classify says what a reply means. A response that arrived is final unless
// it says the platform, not the request, was the problem: 408, 429, a 5xx
// (503 is a model asleep past its wake hold, or no replica yet), or a 200
// that a yield cut short (finish_reason "abort", what the in-flight requests
// of a deployment whose yield policy aborts unfinished work end with when
// another deployment takes its cards). A transport error is retried too.
func classify(r Reply) (v verdict, code, message string) {
	switch {
	case r.TooLarge:
		return verdictFail, "response_too_large", "the response is larger than the gateway keeps for a batch reply"
	case r.Status == 0:
		return verdictRetry, "", ""
	case r.Status >= 200 && r.Status < 300:
		if aborted(r.Body) {
			return verdictRetry, "", ""
		}
		return verdictDone, "", ""
	case r.Status == http.StatusRequestTimeout, r.Status == http.StatusTooManyRequests, r.Status >= 500:
		return verdictRetry, "", ""
	}
	code, message = upstreamError(r)
	return verdictFail, code, message
}

// aborted reports whether a chat or text completion response was cut off by
// a preemption: some choice ends with finish_reason "abort".
func aborted(body []byte) bool {
	if !bytes.Contains(body, []byte("abort")) {
		return false
	}
	var doc struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	for _, c := range doc.Choices {
		if c.FinishReason == "abort" {
			return true
		}
	}
	return false
}

// upstreamError reads the code and message of an error response.
func upstreamError(r Reply) (code, message string) {
	var doc struct {
		Error struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.Body, &doc)
	switch c := doc.Error.Code.(type) {
	case string:
		code = c
	case float64:
		code = fmt.Sprint(int(c))
	}
	if code == "" {
		code = doc.Error.Type
	}
	if code == "" {
		code = fmt.Sprintf("http_%d", r.Status)
	}
	message = doc.Error.Message
	if message == "" {
		message = clip(strings.TrimSpace(string(r.Body)), 300)
	}
	if message == "" {
		message = http.StatusText(r.Status)
	}
	return code, message
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// backoff is the wait before attempt number attempt+1 (attempt counts from
// 0): base, doubling, at most limit, and no shorter than the delay the reply
// asked for (also capped at limit).
func backoff(attempt int, base, limit, asked time.Duration) time.Duration {
	d := base
	for i := 0; i < attempt && d < limit; i++ {
		d *= 2
	}
	d = min(d, limit)
	return min(max(d, asked), limit)
}
