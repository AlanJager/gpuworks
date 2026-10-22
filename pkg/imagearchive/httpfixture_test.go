package imagearchive

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// closingListener lets the regression hold handler accounting until the
// fixture explicitly closes its server, after the client has read the body.
type closingListener struct {
	net.Listener
	closed chan struct{}
	once   sync.Once
}

func (l *closingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

type heldWriteResponse struct {
	http.ResponseWriter
	release <-chan struct{}
}

func (w heldWriteResponse) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	w.ResponseWriter.(http.Flusher).Flush()
	<-w.release
	return n, err
}

// rangeServer is a faithful static-file server: HEAD reports Content-Length and
// Accept-Ranges, GET with a Range header answers 206 with a correct
// Content-Range, GET without one answers 200 with the whole body.
type rangeServer struct {
	body           []byte
	noAcceptRanges bool

	mu     sync.Mutex
	reqs   []string // "METHOD Range-header"
	served int64    // body bytes written to clients
}

func newRangeServer(t *testing.T, body []byte, opts ...func(*rangeServer)) (*rangeServer, *httptest.Server) {
	t.Helper()
	rs := &rangeServer{body: body}
	for _, o := range opts {
		o(rs)
	}
	srv := httptest.NewServer(rs)
	t.Cleanup(srv.Close)
	return rs, srv
}

func withoutAcceptRanges(rs *rangeServer) { rs.noAcceptRanges = true }

func (rs *rangeServer) record(method, rangeHdr string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.reqs = append(rs.reqs, strings.TrimSpace(method+" "+rangeHdr))
}

func (rs *rangeServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.reqs...)
}

func (rs *rangeServer) bytesServed() int64 {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.served
}

func (rs *rangeServer) addServed(n int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.served += int64(n)
}

func (rs *rangeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	total := int64(len(rs.body))
	rangeHdr := r.Header.Get("Range")
	h := w.Header()
	h.Set("Content-Type", "application/x-tar")
	h.Set("ETag", `"fixture-etag"`)
	if !rs.noAcceptRanges {
		h.Set("Accept-Ranges", "bytes")
	}
	switch r.Method {
	case http.MethodHead:
		h.Set("Content-Length", strconv.FormatInt(total, 10))
		rs.record(r.Method, rangeHdr)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if rangeHdr == "" {
			h.Set("Content-Length", strconv.FormatInt(total, 10))
			rs.record(r.Method, rangeHdr)
			w.WriteHeader(http.StatusOK)
			n, _ := w.Write(rs.body)
			rs.addServed(n)
			return
		}
		start, end, ok := parseRangeHeader(rangeHdr, total)
		if !ok {
			h.Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			rs.record(r.Method, rangeHdr)
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		rs.record(r.Method, rangeHdr)
		w.WriteHeader(http.StatusPartialContent)
		n, _ := w.Write(rs.body[start : end+1])
		rs.addServed(n)
	default:
		rs.record(r.Method, rangeHdr)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// parseRangeHeader understands the single "bytes=start-end" / "bytes=start-"
// form this package emits.
func parseRangeHeader(v string, total int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(v), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	if first == "" { // suffix range: bytes=-N
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > total {
			n = total
		}
		return total - n, total - 1, true
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false
	}
	end = total - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end > total-1 {
			end = total - 1
		}
	}
	return start, end, true
}

// floodServer ignores Range entirely and streams the whole body with 200, the
// behaviour of servers that advertise no Accept-Ranges and honour no Range.
type floodServer struct {
	size int64

	mu      sync.Mutex
	written int64
	done    chan struct{}
	once    sync.Once
}

func newFloodServer(t *testing.T, size int64) (*floodServer, *httptest.Server) {
	t.Helper()
	fs := &floodServer{size: size, done: make(chan struct{})}
	srv := httptest.NewServer(fs)
	t.Cleanup(srv.Close)
	return fs, srv
}

func (fs *floodServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", strconv.FormatInt(fs.size, 10))
	w.Header().Set("Content-Type", "application/x-tar")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	defer fs.once.Do(func() { close(fs.done) })
	w.WriteHeader(http.StatusOK)
	chunk := make([]byte, 32<<10)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	var sent int64
	for sent < fs.size {
		n, err := w.Write(chunk)
		fs.mu.Lock()
		fs.written += int64(n)
		fs.mu.Unlock()
		sent += int64(n)
		if err != nil {
			return
		}
	}
}

// bytesWritten waits for the streaming handler to stop, then reports how much
// of the body it managed to write.
func (fs *floodServer) bytesWritten(t *testing.T) int64 {
	t.Helper()
	select {
	case <-fs.done:
	case <-time.After(10 * time.Second):
		t.Fatalf("streaming handler did not finish")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.written
}
