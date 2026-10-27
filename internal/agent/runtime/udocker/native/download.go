package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Timing of a blob transfer; variables so tests can shrink them.
var (
	// idleTimeout fires when a connection delivers nothing for this long. The
	// HTTP client has no overall deadline (multi-gigabyte blobs on throttled
	// links legitimately take many minutes), so this is what distinguishes a
	// slow transfer from a dead one.
	idleTimeout = 2 * time.Minute
	// slowWindow and slowMinRate recycle a connection that is alive but
	// crawling: one that moved less than slowMinRate bytes per second on
	// average over slowWindow. A CDN connection that degrades to tens of KB/s
	// rarely recovers on its own, while a fresh one usually runs at line rate
	// again, so waiting for the idle timeout (which a trickle never trips) is
	// the wrong call. The floor is per connection, and a pull runs up to
	// Workers x RangeParts connections at once (24 by default), so it has to
	// sit far below a slow link's share: 16 KiB/s each is still under
	// 400 KiB/s in total, which keeps a node that is merely slow (mainland to
	// an overseas CDN at peak is tens to hundreds of KB/s) from recycling
	// every connection every window, while a connection that has collapsed to
	// a trickle is still replaced. The transfer resumes where it stopped (see
	// fetchSpan), so recycling loses nothing.
	slowWindow  = 30 * time.Second
	slowMinRate = int64(16 << 10)
	// retryPause is the unit of the pause before a reconnect that follows a
	// failure which delivered nothing; it grows with consecutive such failures.
	retryPause = time.Second
)

const (
	// blobAttempts bounds consecutive failures that make no headway, both for
	// one connection's resumes and for a whole blob.
	blobAttempts = 3
)

var (
	errIdle = errors.New("registry download stalled")
	// errSlow marks a connection recycled for crawling (see slowMinRate).
	errSlow = errors.New("registry download too slow")
)

// Downloader fetches blobs into a content-addressed directory. Layers are
// fetched concurrently and a single large layer is additionally split into
// byte ranges, because one TCP stream rarely saturates the link on the
// throttled nodes this driver targets.
type Downloader struct {
	Client     *Client
	Dir        string // layer store; files are named "sha256:<hex>"
	Workers    int    // concurrent layers
	RangeParts int    // parts a large blob is split into
	RangeMin   int64  // blobs at least this large are split
	Chown      *Owner // when set, blobs are handed to this uid/gid

	// Stream, when set, is invoked concurrently with each blob's download and
	// receives that blob's bytes in order as they arrive. It exists so the CPU
	// can work on a layer while the network is still the bottleneck: during a
	// pull the cores are otherwise idle, and inflating a layer is the single
	// most expensive part of creating a container from it.
	//
	// The bytes are not yet verified when Stream sees them, so a consumer must
	// treat its output as provisional; the downloader reports whether the blob
	// went on to pass its digest check through the returned commit callback.
	// A Stream error never fails the download -- the work it does is an
	// optimization -- and a retried transfer restarts it from the beginning.
	Stream func(ctx context.Context, desc Descriptor, r io.Reader) (commit func(verified bool), err error)

	// StreamWant filters which blobs Stream is offered. It is consulted before
	// a download starts so the prefix bookkeeping is only set up for blobs that
	// will actually use it -- most layers in an image are small enough not to
	// be worth streaming. Nil means every blob is offered.
	StreamWant func(Descriptor) bool

	// Fetched, when set, is called once per descriptor as soon as its blob is
	// in the layer store and verified (immediately for blobs already there),
	// so work that needs whole blobs can start before the last one arrives.
	// It must be safe for concurrent use.
	Fetched func(Descriptor)
}

// streaming reports whether desc should be handed to Stream as it downloads.
func (d *Downloader) streaming(desc Descriptor) bool {
	return d.Stream != nil && (d.StreamWant == nil || d.StreamWant(desc))
}

// Owner is the uid/gid that must own everything written, so that udocker --
// which the driver runs as a dedicated non-root user -- can read and replace
// what the agent (usually root) produced.
type Owner struct {
	UID int
	GID int
}

// BlobPath is where digest's content lives in the layer store.
func (d *Downloader) BlobPath(digest string) string {
	return filepath.Join(d.Dir, digest)
}

// Present reports whether the blob is already on disk at its expected size.
// Content is trusted on size match: the file only ever appears through an
// atomic rename after full sha256 verification.
func (d *Downloader) Present(desc Descriptor) bool {
	info, err := os.Stat(d.BlobPath(desc.Digest))
	return err == nil && info.Mode().IsRegular() && info.Size() == desc.Size
}

// FetchAll downloads every missing descriptor concurrently. progress is called
// with the number of bytes just written for a digest; it must be safe for
// concurrent use.
func (d *Downloader) FetchAll(ctx context.Context, ref Ref, descs []Descriptor, progress func(digest string, n int64), restart func(digest string)) error {
	if err := os.MkdirAll(d.Dir, 0o755); err != nil {
		return err
	}
	workers := d.Workers
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for _, desc := range descs {
		if d.Present(desc) {
			if progress != nil {
				progress(desc.Digest, desc.Size)
			}
			if d.Fetched != nil {
				d.Fetched(desc)
			}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(desc Descriptor) {
			defer wg.Done()
			defer func() { <-sem }()
			err := d.fetch(ctx, ref, desc, progress, restart)
			if err == nil && d.Fetched != nil {
				d.Fetched(desc)
			}
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("layer %s: %w", shortDigest(desc.Digest), err)
				}
				mu.Unlock()
			}
		}(desc)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if d.Chown != nil {
		for _, desc := range descs {
			_ = os.Lchown(d.BlobPath(desc.Digest), d.Chown.UID, d.Chown.GID)
		}
	}
	return ctx.Err()
}

func (d *Downloader) fetch(ctx context.Context, ref Ref, desc Descriptor, progress func(string, int64), restart func(string)) error {
	var lastErr error
	for attempt := 1; attempt <= blobAttempts; attempt++ {
		if attempt > 1 {
			if restart != nil {
				restart(desc.Digest)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt-1) * time.Second):
			}
		}
		lastErr = d.fetchOnce(ctx, ref, desc, progress)
		if lastErr == nil || ctx.Err() != nil || !retryable(lastErr) {
			return lastErr
		}
	}
	return lastErr
}

func (d *Downloader) fetchOnce(ctx context.Context, ref Ref, desc Descriptor, progress func(string, int64)) error {
	tmp, err := os.CreateTemp(d.Dir, ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	url := d.Client.BlobURL(ref, desc.Digest)
	report := func(n int64) {
		if progress != nil {
			progress(desc.Digest, n)
		}
	}
	ranged := d.RangeMin > 0 && desc.Size >= d.RangeMin && d.RangeParts > 1
	parts, chunk := 1, desc.Size
	if ranged {
		parts = d.RangeParts
		chunk = (desc.Size + int64(parts) - 1) / int64(parts)
	}

	// When a consumer wants the bytes as they arrive, track the contiguous
	// prefix so it can read sequentially even though range parts land out of
	// order, and hand it a reader that follows that prefix.
	var growth *growingFile
	var stream *streamHandle
	if d.streaming(desc) {
		growth = newGrowingFile(desc.Size, chunk, parts)
		stream = d.startStream(ctx, desc, tmpName, growth)
		defer stream.stop()
	}
	advance := func(part int, n int64) {
		if growth != nil {
			growth.advance(part, n)
		}
	}
	if ranged {
		err = d.fetchRanged(ctx, ref, url, desc, tmp, chunk, report, advance)
	} else {
		err = d.fetchWhole(ctx, ref, url, desc.Size, tmp, report, advance)
	}
	if growth != nil {
		growth.finish(err)
	}
	if err != nil {
		return err
	}
	if info, statErr := tmp.Stat(); statErr != nil || info.Size() != desc.Size {
		return fmt.Errorf("size mismatch: got %d want %d", fileSize(tmp), desc.Size)
	}
	// Verifying after the write (rather than hashing the stream) keeps the
	// ranged path -- whose parts arrive out of order -- on the same code path
	// as the whole-blob one, and reads back from page cache.
	if err := verifyDigest(tmpName, desc.Digest); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, d.BlobPath(desc.Digest)); err != nil {
		return err
	}
	if stream != nil {
		// The blob is verified and in place, so whatever the consumer derived
		// from it may now be published.
		stream.finish()
	}
	return nil
}

func (d *Downloader) fetchWhole(ctx context.Context, ref Ref, url string, size int64, dst *os.File, report func(int64), advance func(int, int64)) error {
	return d.fetchSpan(ctx, ref, url, dst, 0, size-1, false, report, func(n int64) { advance(0, n) })
}

// advanceWriter reports each completed write so a growingFile's contiguous
// prefix only ever covers bytes already durable in the file.
type advanceWriter struct {
	w       io.Writer
	part    int
	advance func(int, int64)
}

func (w *advanceWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if n > 0 && w.advance != nil {
		w.advance(w.part, int64(n))
	}
	return n, err
}

// fetchRanged splits one blob across RangeParts connections, each writing at
// its own offset in the preallocated temp file, so no concatenation pass
// touches the disk afterwards.
func (d *Downloader) fetchRanged(ctx context.Context, ref Ref, url string, desc Descriptor, dst *os.File, chunk int64, report func(int64), advance func(int, int64)) error {
	if err := dst.Truncate(desc.Size); err != nil {
		return err
	}
	parts := int64(d.RangeParts)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for index := int64(0); index < parts; index++ {
		start := index * chunk
		if start >= desc.Size {
			break
		}
		end := start + chunk - 1
		if end >= desc.Size {
			end = desc.Size - 1
		}
		wg.Add(1)
		go func(part int, start, end int64) {
			defer wg.Done()
			err := d.fetchSpan(ctx, ref, url, dst, start, end, true, report, func(n int64) { advance(part, n) })
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel() // abandon sibling parts; the whole blob is retried
				}
				mu.Unlock()
			}
		}(int(index), start, end)
	}
	wg.Wait()
	return firstErr
}

// fetchSpan downloads bytes start..end (inclusive) of url into dst at the same
// offsets, and keeps going across connections until it has them all: a
// connection that dies, stalls or crawls is replaced by a new one that asks
// for only the bytes still missing, so a bad connection costs a reconnect and
// not the gigabytes already transferred. It gives up after blobAttempts
// consecutive connections that delivered nothing. ranged says the span is one
// part of a split blob, which a registry must serve as such; a whole blob
// (ranged false) is first asked for plainly, since some registries are
// fussier about Range than about a plain GET. end < start means the length is
// unknown (an empty descriptor) and nothing is resumed.
func (d *Downloader) fetchSpan(ctx context.Context, ref Ref, url string, dst *os.File, start, end int64, ranged bool, report func(int64), advance func(int64)) error {
	var got int64
	stuck := 0
	for {
		n, err := d.spanOnce(ctx, ref, url, dst, start, end, got, ranged, report, advance)
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
			stuck = 0
		case errors.Is(err, errSlow):
			// A recycled connection that moved nothing at all is no worse than
			// the others; the next one may be better.
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
// bytes of it are already in dst, and returns how many bytes it added.
func (d *Downloader) spanOnce(ctx context.Context, ref Ref, url string, dst *os.File, start, end, got int64, ranged bool, report func(int64), advance func(int64)) (int64, error) {
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
				// retry fall back.
				return 0, fmt.Errorf("registry ignored range request")
			}
			skip = offset
		}
	default:
		if headers != nil {
			return 0, fmt.Errorf("blob range %d-%d: %s", offset, end, resp.Status)
		}
		return 0, fmt.Errorf("blob: %s", resp.Status)
	}
	want := int64(-1)
	if end >= start {
		want = end - offset + 1
	}
	var seen int64
	reader := newIdleReader(ctx, cancelRequest, resp.Body, func(n int64) {
		// Bytes thrown away were already counted when they first arrived.
		counted := n - min(n, max(skip-seen, 0))
		seen += n
		if counted > 0 && report != nil {
			report(counted)
		}
	})
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, reader, skip); err != nil {
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

func verifyDigest(path, digest string) error {
	algo, want, ok := strings.Cut(digest, ":")
	if !ok || algo != "sha256" {
		return fmt.Errorf("unsupported digest %q", digest)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var hasher hash.Hash = sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return err
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
		return fmt.Errorf("digest mismatch: got sha256:%s want %s", got, digest)
	}
	return nil
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errIdle) || errors.Is(err, errSlow) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	text := err.Error()
	for _, marker := range []string{
		"connection reset", "unexpected EOF", "broken pipe", "TLS handshake",
		"timeout", "temporary failure", "no such host", "EOF",
		"size mismatch", "digest mismatch", "ignored range request",
		": 429", ": 500", ": 502", ": 503", ": 504",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func fileSize(f *os.File) int64 {
	if info, err := f.Stat(); err == nil {
		return info.Size()
	}
	return -1
}

func shortDigest(digest string) string {
	digest = strings.TrimPrefix(digest, "sha256:")
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}
