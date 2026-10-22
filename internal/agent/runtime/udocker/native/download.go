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

const (
	blobAttempts = 3
	// idleTimeout fires when a connection delivers nothing for this long. The
	// HTTP client has no overall deadline (multi-gigabyte blobs on throttled
	// links legitimately take many minutes), so this is what distinguishes a
	// slow transfer from a dead one.
	idleTimeout = 2 * time.Minute
)

var errIdle = errors.New("registry download stalled")

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
		err = d.fetchWhole(ctx, ref, url, tmp, report, advance)
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

func (d *Downloader) fetchWhole(ctx context.Context, ref Ref, url string, dst *os.File, report func(int64), advance func(int, int64)) error {
	resp, err := d.Client.get(ctx, ref, url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob: %s", resp.Status)
	}
	// The write must be recorded after it lands, not when the bytes arrive, so
	// a streaming consumer never reads past what is on disk.
	writer := &advanceWriter{w: dst, part: 0, advance: advance}
	_, err = io.Copy(writer, newIdleReader(ctx, resp.Body, report))
	return err
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
			err := d.fetchRange(ctx, ref, url, dst, start, end, report, func(n int64) { advance(part, n) })
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

func (d *Downloader) fetchRange(ctx context.Context, ref Ref, url string, dst *os.File, start, end int64, report func(int64), advance func(int64)) error {
	resp, err := d.Client.get(ctx, ref, url, map[string]string{
		"Range": fmt.Sprintf("bytes=%d-%d", start, end),
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		// The registry ignored the range header and is sending the whole blob.
		// Only the part that starts at zero can use it; the others would
		// duplicate the transfer, so fail and let the retry fall back.
		if start != 0 {
			return fmt.Errorf("registry ignored range request")
		}
	default:
		return fmt.Errorf("blob range %d-%d: %s", start, end, resp.Status)
	}
	writer := &advanceWriter{w: &sectionWriter{file: dst, offset: start}, advance: func(_ int, n int64) { advance(n) }}
	_, err = io.Copy(writer, newIdleReader(ctx, resp.Body, report))
	return err
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

// idleReader aborts a transfer that delivers nothing for idleTimeout, and
// reports bytes as they arrive so progress reflects the network rather than
// the writer.
type idleReader struct {
	inner  io.Reader
	ctx    context.Context
	report func(int64)
	timer  *time.Timer
	cancel context.CancelFunc
}

func newIdleReader(ctx context.Context, inner io.Reader, report func(int64)) io.Reader {
	ctx, cancel := context.WithCancel(ctx)
	r := &idleReader{inner: inner, ctx: ctx, report: report, cancel: cancel}
	r.timer = time.AfterFunc(idleTimeout, cancel)
	return r
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		r.timer.Reset(idleTimeout)
		if r.report != nil {
			r.report(int64(n))
		}
	}
	if err == nil && r.ctx.Err() != nil {
		err = errIdle
	}
	if err != nil {
		r.timer.Stop()
		r.cancel()
		if errors.Is(r.ctx.Err(), context.Canceled) && err == io.ErrUnexpectedEOF {
			err = errIdle
		}
	}
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
	if errors.Is(err, errIdle) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
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
