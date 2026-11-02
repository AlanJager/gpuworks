package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RequestRecorder keeps the raw exchanges of the requests a debug session
// records (diagnostics). Begin returns where one request's exchange is
// written, or nil when its deployment is not being recorded; the writer
// bounds what it keeps and is closed once the exchange has ended.
type RequestRecorder interface {
	Begin(deploymentID uint, requestID string) io.WriteCloser
}

// SetRequestRecorder wires debug-session request recording; nil turns it off.
// Set it before serving.
func (p *Proxy) SetRequestRecorder(r RequestRecorder) { p.recorder = r }

// exchangeCapture writes one request's exchange as JSON lines: the request as
// sent to the engine, the reply's status and headers, every chunk read from
// the engine ("upstream") and written to the client ("client") with its time
// since dispatch, and how the exchange ended. Chunk data is the bytes as text,
// which an SSE stream is.
type exchangeCapture struct {
	mu      sync.Mutex
	w       io.WriteCloser
	started time.Time
	closed  bool
}

func newExchangeCapture(w io.WriteCloser, started time.Time) *exchangeCapture {
	return &exchangeCapture{w: w, started: started}
}

func (c *exchangeCapture) event(fields map[string]any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	fields["at_ms"] = float64(time.Since(c.started).Microseconds()) / 1000
	line, err := json.Marshal(fields)
	if err != nil {
		return
	}
	_, _ = c.w.Write(append(line, '\n'))
}

func (c *exchangeCapture) chunk(direction string, p []byte) {
	if c == nil || len(p) == 0 {
		return
	}
	c.event(map[string]any{"t": direction, "data": string(p)})
}

// end writes the exchange's last line and closes it; later calls do nothing.
func (c *exchangeCapture) end(fields map[string]any) {
	if c == nil {
		return
	}
	fields["t"] = "end"
	c.event(fields)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		_ = c.w.Close()
	}
}

// capturedHeaders is h without the credentials a caller or the platform sent.
func capturedHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		switch strings.ToLower(name) {
		case "authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key":
			out[name] = []string{"[redacted]"}
		default:
			out[name] = values
		}
	}
	return out
}

// captureRequest writes the request as it goes to the engine: the body is
// read again from r.GetBody, which the gateway always sets on a routed
// request (it may have rewritten the body, injectUsage).
func (c *exchangeCapture) captureRequest(r *http.Request, maxBody int64) {
	if c == nil {
		return
	}
	fields := map[string]any{"t": "request", "method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery, "headers": capturedHeaders(r.Header)}
	if r.GetBody != nil {
		if body, err := r.GetBody(); err == nil {
			b, _ := io.ReadAll(io.LimitReader(body, maxBody))
			_ = body.Close()
			fields["body"] = string(b)
		}
	}
	c.event(fields)
}

// captureBody records the engine's reply as the gateway reads it.
type captureBody struct {
	io.ReadCloser
	capture *exchangeCapture
}

func (b *captureBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.capture.chunk("upstream", p[:n])
	return n, err
}

// captureWriter records what the client is sent.
type captureWriter struct {
	http.ResponseWriter
	capture *exchangeCapture
}

func (w *captureWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture.chunk("client", p[:n])
	return n, err
}

func (w *captureWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *captureWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
