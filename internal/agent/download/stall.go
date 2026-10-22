package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// ErrStalled is a request that delivered nothing for stallTimeout: no
// response headers, or no body bytes. A caller that can fetch the same file
// elsewhere counts these per address; the transfer resumes from its .part
// file wherever it continues.
var ErrStalled = errors.New("download stalled")

// errStalled reports a request that delivered nothing for stallTimeout. It
// wraps ErrStalled, and context.DeadlineExceeded so a retry classifier treats
// it as the transient transport timeout it is.
func errStalled(after time.Duration) error {
	return fmt.Errorf("download: no data received for %s: %w: %w", after, ErrStalled, context.DeadlineExceeded)
}

// guardedGet issues a GET for opts.URL (with rangeHeader when non-empty) and
// returns the response with a body that fails with errStalled once stallTimeout
// passes without a byte. The same limit covers the wait for response headers.
// Closing the body releases the request.
func guardedGet(ctx context.Context, opts Options, rangeHeader string) (*http.Response, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	stall := opts.stallTimeout()
	headerWait := time.AfterFunc(stall, cancel)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, opts.URL, nil)
	if err != nil {
		headerWait.Stop()
		cancel()
		return nil, err
	}
	applyHeaders(req, opts.Headers)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	resp, err := opts.Client.Do(req)
	if !headerWait.Stop() && ctx.Err() == nil {
		// The timer fired: no response headers within stallTimeout.
		if err == nil {
			resp.Body.Close()
		}
		cancel()
		return nil, errStalled(stall)
	}
	if err != nil {
		cancel()
		return nil, err
	}
	body := &stallBody{inner: resp.Body, cancel: cancel, after: stall}
	body.timer = time.AfterFunc(stall, func() {
		body.stalled.Store(true)
		cancel()
	})
	resp.Body = body
	return resp, nil
}

// stallBody is a response body that is abandoned when no data arrives for
// stallTimeout.
type stallBody struct {
	inner   io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	stalled atomic.Bool
	after   time.Duration
}

func (b *stallBody) Read(p []byte) (int, error) {
	n, err := b.inner.Read(p)
	if b.stalled.Load() {
		return n, errStalled(b.after)
	}
	if n > 0 {
		b.timer.Reset(b.after)
	}
	return n, err
}

func (b *stallBody) Close() error {
	b.timer.Stop()
	err := b.inner.Close()
	b.cancel()
	return err
}
