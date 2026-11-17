package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
)

// replyModelRewriter gives a reply's "model" back the public name the client
// asked for, where the engine echoes the name the gateway asked it for
// instead (rewriteModel). On a host-process node that name is the model's
// host directory, which carries the Mac account's user name and the layout
// of its model store (ADR 0044), so it never reaches the client.
//
// Every "model": "<engine name>" member is rewritten, whole JSON reply or
// each SSE event alike; a value naming anything else is left alone, as is the
// key inside a string, where its quotes are escaped. Bytes pass through as
// they arrive; only a read that ends inside a candidate member holds that
// tail (at most replyModelHoldMax bytes) until the next read completes it.
// The reply's length changes, so its Content-Length is dropped
// (wrapReplyModel).
type replyModelRewriter struct {
	src         io.ReadCloser
	engine      string
	public      []byte // JSON-encoded public name
	in, out     []byte
	err         error
	minValueLen int
}

// replyModelHoldMax bounds a held tail: the key, its colon and a host path
// fit well within it; a longer value is not one the gateway asked for.
const replyModelHoldMax = 4 << 10

var replyModelKey = []byte(`"model"`)

func newReplyModelRewriter(src io.ReadCloser, engineName, publicName string) *replyModelRewriter {
	public, _ := json.Marshal(publicName)
	return &replyModelRewriter{src: src, engine: engineName, public: public, minValueLen: len(engineName) + 2}
}

// wrapReplyModel installs the rewriter on a JSON or event-stream reply when
// the engine was asked for a name other than the public one.
func wrapReplyModel(resp *http.Response, engineName, publicName string) bool {
	if engineName == "" || publicName == "" || engineName == publicName || resp.Body == nil || resp.Body == http.NoBody {
		return false
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "application/json" && mediaType != "text/event-stream" && !strings.HasSuffix(mediaType, "+json") {
		return false
	}
	resp.Body = newReplyModelRewriter(resp.Body, engineName, publicName)
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return true
}

func (m *replyModelRewriter) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(m.out) == 0 {
		if m.err != nil {
			return 0, m.err
		}
		// The caller's buffer takes the read; in and out grow to a read's
		// size once and are reused.
		n, err := m.src.Read(p)
		m.in = append(m.in, p[:n]...)
		m.err = err
		m.rewrite(err != nil)
	}
	n := copy(p, m.out)
	m.out = m.out[n:]
	if len(m.out) == 0 {
		m.out = m.out[:0]
	}
	return n, nil
}

// rewrite moves the final part of in to out, renaming members on the way;
// what may still become a member is kept in in. At the end everything is
// final.
func (m *replyModelRewriter) rewrite(end bool) {
	data := m.in
	from, search := 0, 0
	held := len(data)
	for {
		i := bytes.Index(data[search:], replyModelKey)
		if i < 0 {
			break
		}
		at := search + i
		start, stop, state := m.member(data, at+len(replyModelKey))
		if state == memberIncomplete && !end && len(data)-at <= replyModelHoldMax {
			held = at
			break
		}
		if state == memberMatch {
			m.out = append(m.out, data[from:start]...)
			m.out = append(m.out, m.public...)
			from = stop
		}
		search = max(stop, at+len(replyModelKey))
	}
	if held == len(data) && !end {
		// A tail that could still become the key.
		for k := min(len(replyModelKey)-1, len(data)-from); k > 0; k-- {
			if bytes.HasSuffix(data, replyModelKey[:k]) {
				held = len(data) - k
				break
			}
		}
	}
	held = max(held, from)
	m.out = append(m.out, data[from:held]...)
	m.in = append(m.in[:0], data[held:]...)
}

type memberState int

const (
	memberOther memberState = iota
	memberMatch
	memberIncomplete
)

// member reads the value after a "model" key at i: start and stop bound a
// string value equal to the engine name when state is memberMatch; stop is
// where scanning resumes otherwise.
func (m *replyModelRewriter) member(data []byte, i int) (start, stop int, state memberState) {
	i = skipJSONSpace(data, i)
	if i == len(data) {
		return i, i, memberIncomplete
	}
	if data[i] != ':' {
		return i, i, memberOther
	}
	i = skipJSONSpace(data, i+1)
	if i == len(data) {
		return i, i, memberIncomplete
	}
	if data[i] != '"' {
		return i, i, memberOther
	}
	start = i
	escaped := false
	for j := i + 1; j < len(data); j++ {
		if j-start > replyModelHoldMax {
			return start, j, memberOther
		}
		switch {
		case escaped:
			escaped = false
		case data[j] == '\\':
			escaped = true
		case data[j] == '"':
			var value string
			if j+1-start < m.minValueLen || json.Unmarshal(data[start:j+1], &value) != nil || value != m.engine {
				return start, j + 1, memberOther
			}
			return start, j + 1, memberMatch
		}
	}
	return start, len(data), memberIncomplete
}

func (m *replyModelRewriter) Close() error { return m.src.Close() }

// engineNameFor is the name the request in ctx asked its engine for: the
// one a retry last renamed it to (routeAttempt.modelName), or the target's.
// Empty means the engine was asked for the public name.
func engineNameFor(ctx context.Context) string {
	if a, ok := ctx.Value(attemptKey).(*routeAttempt); ok && a != nil {
		return a.modelName
	}
	tgt, _ := ctx.Value(targetKey).(target)
	return tgt.modelName
}

// publicNameFor is the name the client of the request in ctx asked for;
// empty when the request did not come through serveSelectedTarget.
func publicNameFor(ctx context.Context) string {
	tgt, _ := ctx.Value(targetKey).(target)
	return tgt.publicName
}
