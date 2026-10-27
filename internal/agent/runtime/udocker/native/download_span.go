package native

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
)

// rangeSpanTarget is the span length a blob much larger than RangeParts spans
// is cut to, and maxRangeParts bounds how many spans one blob gets. Spans are
// what the budget's connections take turns on: a blob cut into more spans than
// there are connections lets the pull use more of them as the budget climbs,
// without cutting every blob into hundreds of requests. A variable so tests can
// shrink it.
var rangeSpanTarget int64 = 64 << 20

const (
	maxRangeParts = 64
	// elsewhereAttempts bounds failures of a span that delivered nothing while
	// the pull's other connections made headway.
	elsewhereAttempts = 6 * blobAttempts
	// slowAttempts bounds consecutive connections recycled for crawling before
	// they added a byte.
	slowAttempts = 5
)

// partsFor is how many spans a blob of size is cut into: at least RangeParts,
// and one per rangeSpanTarget bytes beyond that.
func (d *Downloader) partsFor(size int64) int {
	parts := d.RangeParts
	if n := int((size + rangeSpanTarget - 1) / rangeSpanTarget); n > parts {
		parts = min(n, maxRangeParts)
	}
	return parts
}

// rangeWorkers is how many workers read a layer's spans: as many as the pool
// could ever run. The budget's target and its pauses decide how many of them
// hold a connection, and a count fixed here from the target of the moment (a
// backoff, a climb not yet made) would stay low for the whole of a large layer.
func rangeWorkers(parts int, sess *connbudget.Session) int {
	return min(parts, max(1, sess.Ceiling()))
}

// fetchRanged splits one blob into parts spans written at their own offsets in
// the preallocated temp file, so no concatenation pass touches the disk
// afterwards. A worker takes the next span, in order, when it finishes its
// last one; each request still waits for a connection from the budget, so how
// many run at once is the budget's decision, and the spans a streaming
// consumer needs first are the ones started first.
func (d *Downloader) fetchRanged(ctx context.Context, sess *connbudget.Session, ref Ref, url string, desc Descriptor, dst *os.File, parts int, chunk int64, report func(int64), advance func(int, int64)) error {
	if err := dst.Truncate(desc.Size); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	workers := rangeWorkers(parts, sess)
	var next atomic.Int64
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				index := int(next.Add(1) - 1)
				start := int64(index) * chunk
				if index >= parts || start >= desc.Size {
					return
				}
				end := min(start+chunk-1, desc.Size-1)
				err := d.fetchSpan(ctx, sess, ref, url, dst, start, end, true, report, func(n int64) { advance(index, n) })
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
						cancel() // abandon sibling parts; the whole blob is retried
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// fetchSpan downloads bytes start..end (inclusive) of url into dst at the same
// offsets, and keeps going across connections until it has them all: a
// connection that dies, stalls or crawls is replaced by a new one that asks
// for only the bytes still missing, so a bad connection costs a reconnect and
// not the gigabytes already transferred. It gives up after blobAttempts
// consecutive connections that delivered nothing, or throttleAttempts
// consecutive "slow down" answers. ranged says the span is one part of a split
// blob, which a registry must serve as such; a whole blob (ranged false) is
// first asked for plainly, since some registries are fussier about Range than
// about a plain GET. end < start means the length is unknown (an empty
// descriptor) and nothing is resumed.
func (d *Downloader) fetchSpan(ctx context.Context, sess *connbudget.Session, ref Ref, url string, dst *os.File, start, end int64, ranged bool, report func(int64), advance func(int64)) error {
	var got int64
	stuck, throttled, elsewhere, slowStuck := 0, 0, 0, 0
	for {
		before := sess.Bytes()
		n, err := d.spanOnce(ctx, sess, ref, url, dst, start, end, got, ranged, report, advance)
		got += n
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || end < start || !retryable(err) {
			return err
		}
		var pause time.Duration
		switch {
		case n > 0:
			stuck, throttled, slowStuck = 0, 0, 0
		case errors.Is(err, errSlow):
			// A recycled connection that moved nothing at all is no worse than
			// the others; the next one may be better. But a replacement that
			// is recycled again before it added a byte (a resume answered with
			// the whole blob spends its windows skipping the part already held)
			// is not going to get anywhere: the blob is given up on, and the
			// caller tries another way.
			if slowStuck++; slowStuck >= slowAttempts {
				return err
			}
		case errors.Is(err, errRangeIgnored):
			// Not a failure of the connection: the blob is retried whole.
			return err
		case isThrottled(err):
			// The registry asked for time; the budget makes the next request
			// wait it out (Retry-After, or a doubling pause) before it starts.
			if throttled++; throttled >= throttleAttempts {
				return err
			}
		case sess.Bytes() > before:
			// This connection delivered nothing, but the pull's others did:
			// it was one connection refused or reset (a per-address connection
			// limit does exactly this), not a registry that is down. It waits
			// for a connection the budget will give it; only a long run of
			// these gives up.
			if elsewhere++; elsewhere >= elsewhereAttempts {
				return err
			}
			pause = retryPause
		default:
			stuck++
			if stuck >= blobAttempts {
				return err
			}
			pause = time.Duration(stuck) * retryPause
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pause):
			}
		}
	}
}

// spanOnce makes one connection's attempt at the rest of a span, after got
// bytes of it are already in dst, and returns how many bytes it added. The
// connection holds a budget lease from before the request until the body is
// done.
func (d *Downloader) spanOnce(ctx context.Context, sess *connbudget.Session, ref Ref, url string, dst *os.File, start, end, got int64, ranged bool, report func(int64), advance func(int64)) (int64, error) {
	lease, err := sess.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer lease.Release()
	offset := start + got
	var headers map[string]string
	if ranged || got > 0 {
		headers = map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", offset, end)}
	}
	// The request has a context of its own so the idle watch can abort it.
	reqCtx, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	resp, err := d.Client.get(reqCtx, ref, url, headers)
	if err != nil {
		reportFailure(sess, ctx, err)
		return 0, err
	}
	defer resp.Body.Close()
	// skip is how much of the body to throw away: a registry that answers a
	// resume with the whole blob (200) sends the part already held first.
	var skip int64
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		if headers != nil && offset != 0 {
			if ranged {
				// The registry ignored the range header and is sending the
				// whole blob. Only the part that starts at zero can use it;
				// the others would duplicate the transfer, so fail and let the
				// retry read the blob over one connection.
				return 0, errRangeIgnored
			}
			skip = offset
		}
	default:
		status := &statusError{status: resp.StatusCode}
		if headers != nil {
			status.msg = fmt.Sprintf("blob range %d-%d: %s", offset, end, resp.Status)
		} else {
			status.msg = fmt.Sprintf("blob: %s", resp.Status)
		}
		if throttles(resp.StatusCode) {
			status.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			sess.Throttled(status.retryAfter)
		}
		return 0, status
	}
	want := int64(-1)
	if end >= start {
		want = end - offset + 1
	}
	var seen int64
	reader := newIdleReader(ctx, cancelRequest, resp.Body, lease, func(n int64) {
		// Bytes thrown away were already counted when they first arrived.
		counted := n - min(n, max(skip-seen, 0))
		seen += n
		if counted > 0 && report != nil {
			report(counted)
		}
	})
	// Reading until an error stops the idle timer; a span that ends short of
	// the end of the body must not leave it running.
	defer reader.stop()
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, reader, skip); err != nil {
			reportFailure(sess, ctx, err)
			return 0, err
		}
	}
	// The write must be recorded after it lands, not when the bytes arrive, so
	// a streaming consumer never reads past what is on disk.
	writer := &advanceWriter{
		w:       &sectionWriter{file: dst, offset: offset},
		advance: func(_ int, n int64) { advance(n) },
	}
	var src io.Reader = reader
	if want >= 0 {
		// Never write past the span, whatever the registry sends.
		src = io.LimitReader(reader, want)
	}
	n, err := io.Copy(writer, src)
	if err == nil && want >= 0 && n < want {
		// The body ended early without an error: also a broken connection.
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		reportFailure(sess, ctx, err)
	}
	return n, err
}

// sectionWriter writes sequentially into a file at a fixed starting offset via
// pwrite, so concurrent range parts share one file handle without seeking.
type sectionWriter struct {
	file   *os.File
	offset int64
}

func (w *sectionWriter) Write(p []byte) (int, error) {
	n, err := w.file.WriteAt(p, w.offset)
	w.offset += int64(n)
	return n, err
}
