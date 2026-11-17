package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
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
	cc *http2.ClientConn
	// slots bounds calls in flight (Spec.MaxConcurrency); lifecycle calls
	// do not take one, so a saturated plugin still answers health.
	slots       chan struct{}
	callTimeout time.Duration
}

func newPluginConn(c net.Conn, maxConcurrency int, callTimeout time.Duration) (*pluginConn, error) {
	cc, err := (&http2.Transport{AllowHTTP: true}).NewClientConn(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return &pluginConn{cc: cc, slots: make(chan struct{}, maxConcurrency), callTimeout: callTimeout}, nil
}

func (c *pluginConn) close() { _ = c.cc.Close() }

// errBusy is returned when every call slot is taken.
var errBusy = fmt.Errorf("pluginhost: every call slot is in use")

// do sends a call that takes a slot; the slot is released when the body is
// closed. A call without a deadline gets the default one.
func (c *pluginConn) do(ctx context.Context, req *http.Request) (*http.Response, error) {
	select {
	case c.slots <- struct{}{}:
	default:
		return nil, errBusy
	}
	cancel := context.CancelFunc(func() {})
	if _, ok := ctx.Deadline(); !ok {
		ctx, cancel = context.WithTimeout(ctx, c.callTimeout)
	}
	release := sync.OnceFunc(func() { cancel(); <-c.slots })
	resp, err := c.roundTrip(ctx, req)
	if err != nil {
		release()
		return nil, err
	}
	resp.Body = &releasingBody{ReadCloser: resp.Body, release: release}
	return resp, nil
}

func (c *pluginConn) roundTrip(ctx context.Context, req *http.Request) (*http.Response, error) {
	req = req.Clone(ctx)
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
	resp, err := c.roundTrip(ctx, req)
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
