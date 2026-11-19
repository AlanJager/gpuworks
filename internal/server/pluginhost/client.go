package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// pluginAuthority is the authority of every request to a plugin; the
// connection decides who answers.
const pluginAuthority = "gwx-plugin"

// maxLifecycleBody bounds the bodies of initialize, health and errors.
const maxLifecycleBody = 1 << 20

// pluginConn is the host's HTTP/2 client on a plugin's serve connection.
type pluginConn struct {
	cc *http2.ClientConn //nolint:staticcheck // SA1019: the plugin connection is an HTTP/2 conversation on an already-open socket pair; moving to net/http's ClientConn and Server is a change of its own.
	// plain and stream bound the calls in flight (Spec.MaxConcurrency
	// each), so open streams never take the slots of short calls;
	// lifecycle calls take no slot, so a saturated plugin still answers
	// health.
	plain, stream slotPool
	callTimeout   time.Duration
	// meter watches the connection for a plugin that stopped reading it
	// (stall.go).
	meter *stallMeter
}

func newPluginConn(c net.Conn, maxConcurrency int, callTimeout time.Duration) (*pluginConn, error) {
	meter := newStallMeter(nil)
	cc, err := (&http2.Transport{AllowHTTP: true}).NewClientConn(&meteredConn{Conn: c, m: meter, scan: newFrameScan()}) //nolint:staticcheck // SA1019: the plugin connection is an HTTP/2 conversation on an already-open socket pair; moving to net/http's ClientConn and Server is a change of its own.
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	reserve := AdminReserve(maxConcurrency)
	return &pluginConn{cc: cc, plain: slotPool{limit: maxConcurrency, reserve: reserve},
		stream: slotPool{limit: maxConcurrency, reserve: reserve}, callTimeout: callTimeout, meter: meter}, nil
}

func (c *pluginConn) close() { _ = c.cc.Close() }

// AdminReserve is the share of a pool's slots (Spec.MaxConcurrency) that
// only admin calls may take: a quarter, at least one, and none when the
// pool has a single slot, which members could otherwise never use.
func AdminReserve(maxConcurrency int) int {
	if maxConcurrency < 2 {
		return 0
	}
	return max(1, maxConcurrency/4)
}

// errBusy is returned when every call slot the call's class may take is
// in use.
var errBusy = fmt.Errorf("pluginhost: every call slot is in use")

// errDraining is returned once an idle on-demand plugin is being stopped:
// the call was not sent and waits for the next start.
var errDraining = fmt.Errorf("pluginhost: the plugin is stopping while idle")

// slotPool bounds one kind of call in flight. Member calls may take all
// but reserve of its limit; admin calls may take any slot.
type slotPool struct {
	limit, reserve int

	mu       sync.Mutex
	inUse    int
	members  int
	draining bool
}

// take claims a slot, or says why not (errBusy, errDraining). limit is
// the bound that refused a busy call.
func (p *slotPool) take(admin bool) (limit int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.draining:
		return 0, errDraining
	case p.inUse >= p.limit:
		return p.limit, errBusy
	case !admin && p.members >= p.limit-p.reserve:
		return p.limit - p.reserve, errBusy
	}
	p.inUse++
	if !admin {
		p.members++
	}
	return 0, nil
}

func (p *slotPool) give(admin bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse--
	if !admin {
		p.members--
	}
}

// drainIfIdle refuses every later call (errDraining) when no call holds a
// slot, and reports whether it did; the check and the refusal are one
// step, so no call slips in between.
func (c *pluginConn) drainIfIdle() bool {
	c.plain.mu.Lock()
	defer c.plain.mu.Unlock()
	c.stream.mu.Lock()
	defer c.stream.mu.Unlock()
	if c.plain.inUse+c.stream.inUse > 0 {
		return false
	}
	c.plain.draining, c.stream.draining = true, true
	return true
}

// callError is a call that failed after it took a slot: dispatched says
// whether its request headers had been written to the plugin, so it may
// have acted on it.
type callError struct {
	err        error
	dispatched bool
}

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// do sends a call of class that takes a slot of its pool; the slot is
// released when the body is closed. A call is held to deadline (ctx's
// own when earlier), or the default one when that is zero; a stream call is held
// to deadline only until its response headers arrive, and its body runs
// until ctx ends or the body is closed. touch runs when the slot is
// released. A failed round trip is a *callError.
func (c *pluginConn) do(ctx context.Context, req *http.Request, class Class, deadline time.Time, touch func()) (*http.Response, error) {
	pool := &c.plain
	if class.Stream {
		pool = &c.stream
	}
	if limit, err := pool.take(class.Admin); err != nil {
		return nil, &busyError{err: err, limit: limit}
	}
	if deadline.IsZero() {
		deadline = time.Now().Add(c.callTimeout)
	}
	var cancel context.CancelFunc
	var headers *time.Timer
	if class.Stream {
		ctx, cancel = context.WithCancel(ctx)
		headers = time.AfterFunc(time.Until(deadline), cancel)
	} else {
		ctx, cancel = context.WithDeadline(ctx, deadline)
	}
	// A body is metered while the client writes it, so a plugin that
	// stops reading bodies is noticed (stall.go).
	var body *meteredBody
	if req.Body != nil && req.Body != http.NoBody {
		body = newMeteredBody(req.Body, req.ContentLength, c.meter)
	}
	release := sync.OnceFunc(func() {
		cancel()
		if body != nil {
			body.finish(false)
		}
		pool.give(class.Admin)
		touch()
	})
	var dispatched atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { dispatched.Store(true) }}
	if body != nil {
		trace.WroteRequest = func(info httptrace.WroteRequestInfo) { body.finish(info.Err == nil) }
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	resp, err := c.roundTrip(ctx, req, body)
	if headers != nil && !headers.Stop() {
		// The headers came too late, or not at all.
		if err == nil {
			_ = resp.Body.Close()
		}
		release()
		return nil, &callError{err: fmt.Errorf("%w: no response headers in time", context.DeadlineExceeded), dispatched: dispatched.Load()}
	}
	if err != nil {
		release()
		return nil, &callError{err: err, dispatched: dispatched.Load()}
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

// busyError is a call refused before it took a slot.
type busyError struct {
	err   error
	limit int
}

func (e *busyError) Error() string { return e.err.Error() }
func (e *busyError) Unwrap() error { return e.err }

// roundTrip sends a copy of req; a non-nil body replaces its body.
func (c *pluginConn) roundTrip(ctx context.Context, req *http.Request, body *meteredBody) (*http.Response, error) {
	req = req.Clone(ctx)
	if body != nil {
		req.Body = body
	}
	req.URL.Scheme, req.URL.Host, req.Host = "http", pluginAuthority, pluginAuthority
	req.RequestURI = ""
	if d, ok := ctx.Deadline(); ok {
		req.Header.Set(extapi.HeaderDeadline, strconv.FormatInt(d.UnixMilli(), 10))
	}
	return c.cc.RoundTrip(req)
}

type releasingBody struct {
	io.ReadCloser
	release func()
}

func (b *releasingBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}

// lifecycle sends a gwx/1 lifecycle call (no slot) with a timeout and
// returns the 2xx body; any other answer is an *Error, or a plain error
// when the body is not a gwx/1 error.
func (c *pluginConn) lifecycle(ctx context.Context, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+pluginAuthority+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", extapi.ContentTypeJSON)
	}
	resp, err := c.roundTrip(ctx, req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLifecycleBody+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxLifecycleBody {
		return nil, fmt.Errorf("pluginhost: %s answered more than %d bytes", path, maxLifecycleBody)
	}
	if resp.StatusCode/100 != 2 {
		w, derr := extapi.DecodeWireError(raw)
		if derr != nil {
			return nil, fmt.Errorf("pluginhost: %s answered %d with a body that is not an error object: %w", path, resp.StatusCode, derr)
		}
		return nil, &Error{Status: resp.StatusCode, Wire: *w}
	}
	return raw, nil
}
