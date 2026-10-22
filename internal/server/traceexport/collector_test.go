package traceexport

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
)

// The fake collector answers like the OpenTelemetry Collector's OTLP/HTTP
// receiver: POST only on /v1/traces, application/json bodies (gzip when the
// request says so), the OTLP/JSON field names - an unknown field is refused,
// which is stricter than protojson and catches a misspelt key - hex ids,
// decimal-string nanoseconds; 200 with an empty partial success, or what the
// test scripts.

type wireRequest struct {
	ResourceSpans []struct {
		Resource struct {
			Attributes []wireAttr `json:"attributes"`
		} `json:"resource"`
		ScopeSpans []struct {
			Scope struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"scope"`
			Spans []wireSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

type wireSpan struct {
	TraceID      string     `json:"traceId"`
	SpanID       string     `json:"spanId"`
	TraceState   string     `json:"traceState"`
	ParentSpanID string     `json:"parentSpanId"`
	Flags        uint32     `json:"flags"`
	Name         string     `json:"name"`
	Kind         int        `json:"kind"`
	Start        string     `json:"startTimeUnixNano"`
	End          string     `json:"endTimeUnixNano"`
	Attributes   []wireAttr `json:"attributes"`
	Events       []struct {
		Time string `json:"timeUnixNano"`
		Name string `json:"name"`
	} `json:"events"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

type wireAttr struct {
	Key   string    `json:"key"`
	Value wireValue `json:"value"`
}

type wireValue struct {
	String *string  `json:"stringValue"`
	Int    *string  `json:"intValue"`
	Double *float64 `json:"doubleValue"`
	Bool   *bool    `json:"boolValue"`
	Array  *struct {
		Values []wireValue `json:"values"`
	} `json:"arrayValue"`
}

func (v wireValue) text() string {
	switch {
	case v.String != nil:
		return *v.String
	case v.Int != nil:
		return *v.Int
	case v.Double != nil:
		return strconv.FormatFloat(*v.Double, 'f', -1, 64)
	case v.Bool != nil:
		return strconv.FormatBool(*v.Bool)
	case v.Array != nil:
		out := "["
		for i, e := range v.Array.Values {
			if i > 0 {
				out += ","
			}
			out += e.text()
		}
		return out + "]"
	}
	return "<empty>"
}

func attrs(as []wireAttr) map[string]string {
	out := map[string]string{}
	for _, a := range as {
		out[a.Key] = a.Value.text()
	}
	return out
}

type reply struct {
	status     int
	body       string
	retryAfter string
}

type fakeCollector struct {
	t      *testing.T
	server *httptest.Server
	mu     sync.Mutex
	got    []wireRequest
	header []http.Header
	// script answers the calls in order; past its end every call gets 200.
	script []reply
	calls  int
	// hold, when set, blocks every call until it is closed.
	hold chan struct{}
}

func newCollector(t *testing.T, script ...reply) *fakeCollector {
	c := &fakeCollector{t: t, script: script}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	return c
}

func (c *fakeCollector) serve(w http.ResponseWriter, r *http.Request) {
	if c.hold != nil {
		select {
		case <-c.hold:
		case <-r.Context().Done():
			return
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	fail := func(status int, msg string) {
		c.t.Errorf("collector refused a request: %s", msg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"code":3,"message":%q}`, msg)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/traces" {
		fail(http.StatusNotFound, r.Method+" "+r.URL.Path)
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		fail(http.StatusUnsupportedMediaType, "content type "+r.Header.Get("Content-Type"))
		return
	}
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			fail(http.StatusBadRequest, "gzip: "+err.Error())
			return
		}
		body = zr
	}
	raw, _ := io.ReadAll(body)
	var req wireRequest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		fail(http.StatusBadRequest, "decode: "+err.Error())
		return
	}
	for _, rs := range req.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, sp := range ss.Spans {
				if !isHex(sp.TraceID, 16) || !isHex(sp.SpanID, 8) || (sp.ParentSpanID != "" && !isHex(sp.ParentSpanID, 8)) {
					fail(http.StatusBadRequest, "ids "+sp.TraceID+"/"+sp.SpanID+"/"+sp.ParentSpanID)
					return
				}
				if _, err := strconv.ParseUint(sp.Start, 10, 64); err != nil {
					fail(http.StatusBadRequest, "start "+sp.Start)
					return
				}
				if _, err := strconv.ParseUint(sp.End, 10, 64); err != nil {
					fail(http.StatusBadRequest, "end "+sp.End)
					return
				}
			}
		}
	}
	c.got = append(c.got, req)
	c.header = append(c.header, r.Header.Clone())
	answer := reply{status: http.StatusOK, body: `{"partialSuccess":{}}`}
	if i := c.calls - 1; i < len(c.script) {
		answer = c.script[i]
	}
	if answer.retryAfter != "" {
		w.Header().Set("Retry-After", answer.retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(answer.status)
	_, _ = io.WriteString(w, answer.body)
}

func isHex(s string, n int) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == n && s == hex.EncodeToString(b)
}

func (c *fakeCollector) spans() []wireSpan {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []wireSpan
	for _, req := range c.got {
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				out = append(out, ss.Spans...)
			}
		}
	}
	return out
}

func (c *fakeCollector) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}
