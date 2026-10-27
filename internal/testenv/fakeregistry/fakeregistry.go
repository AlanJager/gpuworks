// Package fakeregistry is a container registry for tests, faithful to the
// parts a parallel blob downloader meets on private registries (Harbor, SWR,
// ACR) and their CDNs: the /v2/ probe, blob GETs that answer 307 to a signed
// address on a separate blob CDN host, strict Range semantics (206 with
// Content-Range, 416 when unsatisfiable), a rate limit per TCP connection and
// for the whole address, a limit on busy connections per client that is
// answered with 429 (Retry-After) or by resetting the connection, throttled
// answers, resets and stalls in the middle of a body, and a server that
// ignores Range altogether. Everything speaks TLS over HTTP/1.1, as the
// downloader's transport does.
//
// It is test support: nothing outside _test files imports it.
package fakeregistry

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Registry. The zero value serves blobs straight from the
// registry host, without limits.
type Options struct {
	// Redirect makes the registry answer a blob GET with 307 to a signed
	// address on a second host (the blob CDN), valid for SignedTTL (an hour if
	// zero); an expired address answers 403. The limits below apply to the host
	// that serves the bytes: the CDN with Redirect, the registry without.
	Redirect  bool
	SignedTTL time.Duration

	// PerConnRate limits each TCP connection to this many bytes per second, the
	// way one throttled flow is; AggregateRate limits the whole address.
	PerConnRate, AggregateRate int64

	// MaxConns is how many connections one client may have a request in flight
	// on at once. A request that would exceed it is answered with 429 and a
	// Retry-After of RetryAfter seconds (when positive), or, with ResetOverLimit,
	// by resetting the connection, the way a firewall's per-address connection
	// limit does.
	MaxConns       int
	RetryAfter     int
	ResetOverLimit bool

	// IgnoreRange answers every request with the whole blob and status 200;
	// Registry.IgnoreRangeFor does the same for chosen blobs only.
	IgnoreRange bool

	// Latency delays every blob request before its answer starts (the round
	// trip); ConnectLatency is added to the first request on a connection (TCP
	// and TLS set-up, slow start). A link with latency is where a new
	// connection's first moments deliver little.
	Latency, ConnectLatency time.Duration
}

// Fault is what a request is made to suffer instead of being served.
type Fault struct {
	// Status, when set, is the answer (429, 503, 403...); RetryAfter is sent as
	// the Retry-After header when non-empty.
	Status     int
	RetryAfter string
	// AbortAfter, when positive, serves that many body bytes and then resets
	// the connection.
	AbortAfter int64
	// Stall serves nothing, not even headers, and holds the request open until
	// the client gives up. StallAfter, when positive, serves that many body
	// bytes and then holds the connection open and silent.
	Stall      bool
	StallAfter int64
	// Rate, when positive, serves this request's body at this many bytes per
	// second regardless of the other limits (a connection that crawls).
	Rate int64
}

// Request describes a blob request to a fault function.
type Request struct {
	Digest string
	Range  string
	// N counts the blob requests that reached the serving host so far, this one
	// included, starting at 1.
	N int
	// Conn identifies the TCP connection the request arrived on.
	Conn int
}

// Stats is what the serving host observed.
type Stats struct {
	// Requests counts blob requests; Redirects the 307s the registry gave.
	Requests, Redirects int
	// Conns counts TCP connections ever opened; PeakConns the most open at once;
	// PeakBusy the most with a blob request in flight at once.
	Conns, PeakConns, PeakBusy int
	// Bytes is what was written to bodies.
	Bytes int64
	// Throttled counts 429s given for the connection limit (faults are not
	// counted); Reset counts connections reset for it.
	Throttled, Reset int
}

// Registry is a running fake registry.
type Registry struct {
	// Server is the registry host; CDN is the blob CDN host, nil without
	// Redirect.
	Server *httptest.Server
	CDN    *httptest.Server

	opts  Options
	serve *host

	mu          sync.Mutex
	blobs       map[string][]byte
	fault       func(Request) *Fault
	seen        []Request
	ignoreRange map[string]bool

	refuseMu   sync.Mutex
	serving    *httptest.Server
	refusing   bool
	refuseAddr string
	reopened   net.Listener
}

type connInfo struct {
	id     int
	bucket *bucket
	busy   int // requests in flight on this connection
	served int // requests answered on this connection so far
}

type connKey struct{}

// host tracks the connections of one server.
type host struct {
	mu       sync.Mutex
	open     int
	busy     int // connections with a request in flight
	inflight int
	nextConn int
	requests int
	stats    Stats
	agg      *bucket
	body     atomic.Int64
}

// New starts a registry. It is closed with the test.
func New(t interface {
	Helper()
	Cleanup(func())
}, opts Options) *Registry {
	t.Helper()
	r := &Registry{opts: opts, blobs: map[string][]byte{}, ignoreRange: map[string]bool{}}
	newServer := func(h http.Handler, tracked *host) *httptest.Server {
		s := httptest.NewUnstartedServer(h)
		s.TLS = &tls.Config{NextProtos: []string{"http/1.1"}} // HTTP/1.1 only
		if tracked != nil {
			s.Config.ConnContext = tracked.connContext(opts)
			s.Config.ConnState = tracked.connState
		}
		s.StartTLS()
		t.Cleanup(s.Close)
		return s
	}
	content := &host{}
	if opts.AggregateRate > 0 {
		content.agg = newBucket(opts.AggregateRate)
	}
	r.serve = content
	if opts.Redirect {
		r.CDN = newServer(http.HandlerFunc(r.serveCDN), content)
		r.Server = newServer(http.HandlerFunc(r.serveRegistry), nil)
		r.serving = r.CDN
	} else {
		r.Server = newServer(http.HandlerFunc(r.serveRegistry), content)
		r.serving = r.Server
	}
	t.Cleanup(func() {
		r.refuseMu.Lock()
		defer r.refuseMu.Unlock()
		if r.reopened != nil {
			r.reopened.Close()
		}
	})
	return r
}

// AddBlob stores content and returns its digest.
func (r *Registry) AddBlob(content []byte) string {
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	r.mu.Lock()
	r.blobs[digest] = content
	r.mu.Unlock()
	return digest
}

// Host is the registry's host:port, as an image reference names it.
func (r *Registry) Host() string { return strings.TrimPrefix(r.Server.URL, "https://") }

// IgnoreRangeFor makes the registry answer requests for this blob with the whole
// blob and status 200, as one served through a path without Range support does.
func (r *Registry) IgnoreRangeFor(digest string) {
	r.mu.Lock()
	r.ignoreRange[digest] = true
	r.mu.Unlock()
}

// SetRefusing closes the serving host's listener (true) so that new
// connections are refused at TCP level, the way a host over its connection
// limit, or one that is down, refuses them, and listens again on the same
// address (false). Connections already open are not touched.
func (r *Registry) SetRefusing(refuse bool) {
	r.refuseMu.Lock()
	defer r.refuseMu.Unlock()
	if refuse == r.refusing {
		return
	}
	srv := r.serving
	if refuse {
		r.refuseAddr = srv.Listener.Addr().String()
		_ = srv.Listener.Close()
		r.refusing = true
		return
	}
	var ln net.Listener
	var err error
	for i := 0; i < 50; i++ { // the port may linger for a moment
		if ln, err = net.Listen("tcp", r.refuseAddr); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		panic(fmt.Sprintf("fakeregistry: cannot listen again on %s: %v", r.refuseAddr, err))
	}
	r.reopened = tls.NewListener(ln, srv.TLS)
	go func() { _ = srv.Config.Serve(r.reopened) }()
	r.refusing = false
}

// SetFault installs a function consulted for every blob request that reaches
// the serving host; nil removes it. It may return nil to serve normally.
func (r *Registry) SetFault(fn func(Request) *Fault) {
	r.mu.Lock()
	r.fault = fn
	r.mu.Unlock()
}

// Requests returns the blob requests the serving host received, in order.
func (r *Registry) Requests() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Request(nil), r.seen...)
}

// Stats returns what the serving host has seen so far.
func (r *Registry) Stats() Stats {
	r.serve.mu.Lock()
	defer r.serve.mu.Unlock()
	s := r.serve.stats
	s.Bytes = r.serve.body.Load()
	return s
}

// OpenConns is how many TCP connections the serving host has open now.
func (r *Registry) OpenConns() int {
	r.serve.mu.Lock()
	defer r.serve.mu.Unlock()
	return r.serve.open
}

// BusyConns is how many connections have a blob request in flight now.
func (r *Registry) BusyConns() int {
	r.serve.mu.Lock()
	defer r.serve.mu.Unlock()
	return r.serve.busy
}

// Client returns an HTTP client that trusts the registry's certificate (the
// CDN's is the same test certificate).
func (r *Registry) Client() *http.Client { return r.Server.Client() }

// RootCAs is the pool that trusts the registry's and the CDN's certificate.
func (r *Registry) RootCAs() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(r.Server.Certificate())
	return pool
}

func (h *host) connContext(opts Options) func(context.Context, net.Conn) context.Context {
	return func(ctx context.Context, _ net.Conn) context.Context {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.nextConn++
		info := &connInfo{id: h.nextConn}
		if opts.PerConnRate > 0 {
			info.bucket = newBucket(opts.PerConnRate)
		}
		return context.WithValue(ctx, connKey{}, info)
	}
}

func (h *host) connState(_ net.Conn, state http.ConnState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch state {
	case http.StateNew:
		h.open++
		h.stats.Conns++
		h.stats.PeakConns = max(h.stats.PeakConns, h.open)
	case http.StateClosed, http.StateHijacked:
		h.open--
	}
}

func (r *Registry) serveRegistry(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	switch {
	case path == "/v2/" || path == "/v2":
		w.WriteHeader(http.StatusOK)
	case strings.Contains(path, "/blobs/"):
		_, digest, _ := strings.Cut(path, "/blobs/")
		if r.opts.Redirect {
			r.mu.Lock()
			_, ok := r.blobs[digest]
			r.mu.Unlock()
			if !ok {
				http.NotFound(w, req)
				return
			}
			r.serve.mu.Lock()
			r.serve.stats.Redirects++
			r.serve.mu.Unlock()
			ttl := r.opts.SignedTTL
			if ttl <= 0 {
				ttl = time.Hour
			}
			expires := time.Now().Add(ttl).UnixNano()
			http.Redirect(w, req, fmt.Sprintf("%s/blob/%d/%s", r.CDN.URL, expires, digest), http.StatusTemporaryRedirect)
			return
		}
		r.content(w, req, digest)
	default:
		http.NotFound(w, req)
	}
}

func (r *Registry) serveCDN(w http.ResponseWriter, req *http.Request) {
	rest, ok := strings.CutPrefix(req.URL.Path, "/blob/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	stamp, digest, _ := strings.Cut(rest, "/")
	expires, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || time.Now().UnixNano() > expires {
		http.Error(w, "signature expired", http.StatusForbidden)
		return
	}
	r.content(w, req, digest)
}

func (r *Registry) content(w http.ResponseWriter, req *http.Request, digest string) {
	h := r.serve
	info, _ := req.Context().Value(connKey{}).(*connInfo)
	r.mu.Lock()
	data, ok := r.blobs[digest]
	fault := r.fault
	r.mu.Unlock()
	if !ok {
		http.NotFound(w, req)
		return
	}

	h.mu.Lock()
	if r.opts.MaxConns > 0 && info != nil && info.busy == 0 && h.busy >= r.opts.MaxConns {
		if r.opts.ResetOverLimit {
			h.stats.Reset++
			h.mu.Unlock()
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					if tc, ok := conn.(*tls.Conn); ok {
						if raw, ok := tc.NetConn().(*net.TCPConn); ok {
							_ = raw.SetLinger(0) // RST, not FIN
						}
					}
					conn.Close()
					return
				}
			}
			panic(http.ErrAbortHandler)
		}
		h.stats.Throttled++
		h.mu.Unlock()
		if r.opts.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(r.opts.RetryAfter))
		}
		http.Error(w, "too many connections from your address", http.StatusTooManyRequests)
		return
	}
	if info != nil {
		if info.busy == 0 {
			h.busy++
			h.stats.PeakBusy = max(h.stats.PeakBusy, h.busy)
		}
		info.busy++
	}
	h.requests++
	n := h.requests
	h.stats.Requests++
	h.inflight++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inflight--
		if info != nil {
			if info.busy--; info.busy == 0 {
				h.busy--
			}
		}
		h.mu.Unlock()
	}()

	conn := 0
	if info != nil {
		conn = info.id
	}
	var delay time.Duration = r.opts.Latency
	if info != nil {
		h.mu.Lock()
		if info.served == 0 {
			delay += r.opts.ConnectLatency
		}
		info.served++
		h.mu.Unlock()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return
		}
	}
	rq := Request{Digest: digest, Range: req.Header.Get("Range"), N: n, Conn: conn}
	r.mu.Lock()
	r.seen = append(r.seen, rq)
	r.mu.Unlock()
	var f *Fault
	if fault != nil {
		f = fault(rq)
	}
	if f != nil {
		if f.Stall {
			<-req.Context().Done()
			return
		}
		if f.Status != 0 {
			if f.RetryAfter != "" {
				w.Header().Set("Retry-After", f.RetryAfter)
			}
			http.Error(w, http.StatusText(f.Status), f.Status)
			return
		}
	}

	size := int64(len(data))
	start, end, partial, ok := parseRange(req.Header.Get("Range"), size)
	r.mu.Lock()
	ignore := r.opts.IgnoreRange || r.ignoreRange[digest]
	r.mu.Unlock()
	if ignore {
		start, end, partial, ok = 0, size-1, false, true
	}
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	body := data[start : end+1]
	limit := int64(len(body))
	if f != nil && f.AbortAfter > 0 && f.AbortAfter < limit {
		limit = f.AbortAfter
	}
	stallAt := int64(-1)
	if f != nil && f.StallAfter > 0 && f.StallAfter < limit {
		limit, stallAt = f.StallAfter, f.StallAfter
	}
	var own *bucket
	if f != nil && f.Rate > 0 {
		own = newBucket(f.Rate)
	}
	const slice = 16 << 10
	flusher, _ := w.(http.Flusher)
	for sent := int64(0); sent < limit; {
		n := min(int64(slice), limit-sent)
		var wait time.Duration
		if own != nil {
			wait = own.reserve(int(n))
		} else {
			wait = h.delay(info, int(n))
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-req.Context().Done():
				return
			}
		}
		if _, err := w.Write(body[sent : sent+n]); err != nil {
			return
		}
		h.body.Add(n)
		sent += n
		if flusher != nil {
			flusher.Flush()
		}
	}
	if stallAt >= 0 {
		<-req.Context().Done()
		return
	}
	if limit < int64(len(body)) {
		panic(http.ErrAbortHandler) // reset mid-body
	}
}

// delay is how long the bytes about to be written must wait for the
// connection's and the address's rate limits.
func (h *host) delay(info *connInfo, n int) time.Duration {
	var wait time.Duration
	if info != nil && info.bucket != nil {
		wait = info.bucket.reserve(n)
	}
	if h.agg != nil {
		wait = max(wait, h.agg.reserve(n))
	}
	return wait
}
