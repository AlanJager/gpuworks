package native

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/connbudget"
)

// blobServer is a registry with one blob and a hook into every blob request,
// so a test can make individual connections die, go silent or crawl the way
// CDN connections do. Everything else about it is a registry: it answers the
// /v2/ probe, honours Range with 206 and Content-Range, and sends a correct
// Content-Length, so the Downloader meets the same framing a real one does.
type blobServer struct {
	server *httptest.Server
	data   []byte
	desc   Descriptor
	ref    Ref

	mu       sync.Mutex
	requests []blobRequest
	// misbehave is consulted for each blob request, with its 1-based number
	// among the requests for that range start. Returning false serves normally.
	misbehave func(n int, req blobRequest, w http.ResponseWriter, r *http.Request) (handled bool)
	// ignoreRange makes the server answer every request with 200 and the whole
	// blob, as a registry that does not support Range does.
	ignoreRange bool
}

type blobRequest struct {
	start, end int64
	ranged     bool
}

func newBlobServer(t *testing.T, size int) *blobServer {
	t.Helper()
	data := make([]byte, size)
	rand.New(rand.NewSource(7)).Read(data)
	sum := sha256.Sum256(data)
	s := &blobServer{data: data, desc: Descriptor{Digest: "sha256:" + hex.EncodeToString(sum[:]), Size: int64(size)}}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/blobs/") {
			w.WriteHeader(http.StatusOK)
			return
		}
		s.serveBlob(w, r)
	})
	s.server = httptest.NewTLSServer(mux)
	t.Cleanup(s.server.Close)
	s.ref = ParseRef(strings.TrimPrefix(s.server.URL, "https://") + "/repo/blob:latest")
	return s
}

func (s *blobServer) serveBlob(w http.ResponseWriter, r *http.Request) {
	req := blobRequest{start: 0, end: int64(len(s.data)) - 1}
	if header := r.Header.Get("Range"); header != "" && !s.ignoreRange {
		spec := strings.TrimPrefix(header, "bytes=")
		from, to, _ := strings.Cut(spec, "-")
		req.start, _ = strconv.ParseInt(from, 10, 64)
		req.end, _ = strconv.ParseInt(to, 10, 64)
		req.ranged = true
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	n := 0
	for _, seen := range s.requests {
		if seen.start == req.start {
			n++
		}
	}
	hook := s.misbehave
	s.mu.Unlock()
	if hook != nil && hook(n, req, w, r) {
		return
	}
	s.send(w, req, req.end-req.start+1)
}

// send writes the first n bytes of the requested range with the framing of the
// whole range, so cutting it short is a broken connection, not a short file.
func (s *blobServer) send(w http.ResponseWriter, req blobRequest, n int64) {
	w.Header().Set("Content-Length", strconv.FormatInt(req.end-req.start+1, 10))
	if req.ranged {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", req.start, req.end, len(s.data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	w.Write(s.data[req.start : req.start+n])
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *blobServer) seen() []blobRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]blobRequest(nil), s.requests...)
}

// downloaderFor returns a Downloader for the server with the given split.
func (s *blobServer) downloaderFor(t *testing.T, parts int, rangeMin int64) *Downloader {
	t.Helper()
	client := NewClient()
	client.HTTP = s.server.Client()
	return &Downloader{Client: client, Dir: t.TempDir(), Workers: 2, RangeParts: parts, RangeMin: rangeMin, Tune: instantWaits(nil)}
}

// instantWaits makes the budget's pauses (a registry's Retry-After, or the
// default after a 429/503) cost no real time, and records them.
func instantWaits(record *waitLog) func(*connbudget.SessionConfig) {
	return func(cfg *connbudget.SessionConfig) {
		cfg.Wait = func(ctx context.Context, d time.Duration) error {
			if record != nil {
				record.add(d)
			}
			return ctx.Err()
		}
	}
}

type waitLog struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waitLog) add(d time.Duration) {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
}

func (w *waitLog) all() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.d...)
}

// fetch runs FetchAll for the one blob and returns the bytes reported, the
// restarts reported and the error.
func (s *blobServer) fetch(t *testing.T, d *Downloader) (reported int64, restarts int, err error) {
	t.Helper()
	var bytes, restarted atomic.Int64
	err = d.FetchAll(context.Background(), s.ref, []Descriptor{s.desc},
		func(_ string, n int64) { bytes.Add(n) },
		func(string) { restarted.Add(1) })
	return bytes.Load(), int(restarted.Load()), err
}

// quickRecovery shrinks the connection watch from minutes to milliseconds.
func quickRecovery(t *testing.T) {
	t.Helper()
	oldIdle, oldWindow, oldRate, oldPause := idleTimeout, slowWindow, slowMinRate, retryPause
	idleTimeout, slowWindow, slowMinRate, retryPause = 400*time.Millisecond, 150*time.Millisecond, 200_000, time.Millisecond
	t.Cleanup(func() { idleTimeout, slowWindow, slowMinRate, retryPause = oldIdle, oldWindow, oldRate, oldPause })
}

// A connection that dies mid-blob costs a reconnect for the rest of the blob,
// not the blob: nothing restarts, nothing is counted twice.
func TestDownloadResumesABrokenConnectionWhereItStopped(t *testing.T) {
	quickRecovery(t)
	for name, tc := range map[string]struct {
		parts    int
		rangeMin int64
	}{
		"a whole blob":      {parts: 1, rangeMin: 0},
		"a blob in 4 parts": {parts: 4, rangeMin: 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := newBlobServer(t, 1<<20)
			const served = 64 << 10
			var cuts atomic.Int64
			s.misbehave = func(_ int, req blobRequest, w http.ResponseWriter, _ *http.Request) bool {
				if cuts.Add(1) > int64(tc.parts) {
					return false
				}
				// Each part's first connection delivers 64 KiB of its range, then drops.
				s.send(w, req, min(served, req.end-req.start+1))
				panic(http.ErrAbortHandler)
			}
			reported, restarts, err := s.fetch(t, s.downloaderFor(t, tc.parts, tc.rangeMin))
			if err != nil {
				t.Fatal(err)
			}
			if restarts != 0 {
				t.Fatalf("the blob was restarted %d times; the connection should have resumed", restarts)
			}
			if reported != s.desc.Size {
				t.Fatalf("progress reported %d bytes for a %d byte blob", reported, s.desc.Size)
			}
			for _, req := range s.seen() {
				if req.start == 0 && req.end == s.desc.Size-1 && tc.parts > 1 {
					t.Fatalf("a split blob was requested whole: %+v", req)
				}
			}
			// The reconnect asked for what was left of its range, not all of it.
			resumed := 0
			for _, req := range s.seen() {
				if req.ranged && req.start%(s.desc.Size/int64(max(tc.parts, 1))) == served {
					resumed++
				}
			}
			if resumed == 0 {
				t.Fatalf("no request resumed at the byte where a connection dropped: %+v", s.seen())
			}
		})
	}
}

// A connection that stays up but delivers a trickle never trips the idle
// timeout; it is replaced, and the replacement carries on from the bytes the
// trickle did deliver.
func TestDownloadReplacesAConnectionThatCrawls(t *testing.T) {
	quickRecovery(t)
	for name, tc := range map[string]struct {
		parts    int
		rangeMin int64
	}{
		"a whole blob":      {parts: 1, rangeMin: 0},
		"a blob in 4 parts": {parts: 4, rangeMin: 1},
	} {
		t.Run(name, func(t *testing.T) {
			s := newBlobServer(t, 1<<20)
			// The first connection of the part that starts at the 3/4 mark (or
			// of the whole blob) sends 1 KiB every 10 ms, ~100 KB/s, forever.
			crawlAt := int64(0)
			if tc.parts > 1 {
				crawlAt = s.desc.Size / 4 * 2
			}
			s.misbehave = func(n int, req blobRequest, w http.ResponseWriter, r *http.Request) bool {
				if n > 1 || req.start != crawlAt {
					return false
				}
				if req.ranged {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", req.start, req.end, len(s.data)))
				}
				w.Header().Set("Content-Length", strconv.FormatInt(req.end-req.start+1, 10))
				if req.ranged {
					w.WriteHeader(http.StatusPartialContent)
				}
				for off := req.start; off <= req.end && r.Context().Err() == nil; off += 1024 {
					w.Write(s.data[off:min(off+1024, req.end+1)])
					w.(http.Flusher).Flush()
					time.Sleep(10 * time.Millisecond)
				}
				return true
			}
			began := time.Now()
			reported, restarts, err := s.fetch(t, s.downloaderFor(t, tc.parts, tc.rangeMin))
			if err != nil {
				t.Fatal(err)
			}
			if took := time.Since(began); took > 10*time.Second {
				t.Fatalf("took %s: the crawling connection was waited out", took)
			}
			if restarts != 0 || reported != s.desc.Size {
				t.Fatalf("restarts=%d reported=%d of %d: bytes were lost with the connection", restarts, reported, s.desc.Size)
			}
			resumedPast := false
			for _, req := range s.seen() {
				if req.ranged && req.start > crawlAt && req.start < crawlAt+s.desc.Size/4 {
					resumedPast = true
				}
			}
			if !resumedPast {
				t.Fatalf("the replacement connection did not resume after the trickled bytes: %+v", s.seen())
			}
		})
	}
}

// A connection that goes silent with the socket still open is cut by the idle
// timeout. The Read blocked on it only returns when the request itself is
// cancelled, which is what this pins.
func TestDownloadCutsASilentConnection(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var silenced atomic.Bool
	s.misbehave = func(_ int, req blobRequest, w http.ResponseWriter, r *http.Request) bool {
		if silenced.Swap(true) {
			return false
		}
		s.send(w, req, 100<<10)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		return true
	}
	began := time.Now()
	reported, restarts, err := s.fetch(t, s.downloaderFor(t, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(began); took > 20*time.Second {
		t.Fatalf("took %s to notice a silent connection", took)
	}
	if restarts != 0 || reported != s.desc.Size {
		t.Fatalf("restarts=%d reported=%d of %d", restarts, reported, s.desc.Size)
	}
}

// A registry that does not honour Range answers a resume with the whole blob.
// The part already held is skipped, not written again or counted again.
func TestDownloadResumeAgainstARegistryThatIgnoresRange(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	s.ignoreRange = true
	// ignoreRange hides the Range header from serveBlob, so every request looks
	// like the first for start 0; only the first one is cut.
	var first atomic.Bool
	s.misbehave = func(_ int, req blobRequest, w http.ResponseWriter, _ *http.Request) bool {
		if first.Swap(true) {
			return false
		}
		s.send(w, req, 300<<10)
		panic(http.ErrAbortHandler)
	}
	reported, restarts, err := s.fetch(t, s.downloaderFor(t, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if restarts != 0 || reported != s.desc.Size {
		t.Fatalf("restarts=%d reported=%d of %d", restarts, reported, s.desc.Size)
	}
}

// A split blob whose registry ignores Range is not resumed per part (the other
// parts would duplicate the transfer): the first part that is answered with the
// whole blob teaches the pull the registry cannot do ranges, and the blob is
// read again over one plain connection, at once.
func TestDownloadSplitBlobFallsBackToOneConnectionWhenRangeIsIgnored(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	s.ignoreRange = true
	d := s.downloaderFor(t, 4, 1)
	began := time.Now()
	_, restarts, err := s.fetch(t, d)
	if err != nil {
		t.Fatalf("a registry without Range must still serve the blob whole: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d, want the one that switched to a plain read", restarts)
	}
	if took := time.Since(began); took > 800*time.Millisecond {
		t.Fatalf("took %s: the fallback should not wait out a retry pause", took)
	}
	seen := s.seen()
	if last := seen[len(seen)-1]; last.ranged {
		t.Fatalf("the blob was asked for with Range after the registry showed it ignores it: %+v", seen)
	}
}

// Connections that deliver nothing at all are retried a bounded number of
// times, then the blob fails: resuming must not turn into an endless loop.
func TestDownloadGivesUpAfterConnectionsThatDeliverNothing(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	s.misbehave = func(_ int, _ blobRequest, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusInternalServerError)
		return true
	}
	_, _, err := s.fetch(t, s.downloaderFor(t, 1, 0))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v, want the 500", err)
	}
	if got := len(s.seen()); got != blobAttempts*blobAttempts {
		t.Fatalf("%d requests; want %d (%d resumes for each of %d blob attempts)", got, blobAttempts*blobAttempts, blobAttempts, blobAttempts)
	}
}

// A registry that keeps saying "slow down" (503) is waited on, not hammered:
// each answer makes the next request wait (a pause that doubles between separate
// events when the registry names no time), and after throttleAttempts the pull gives up without
// restarting the blob, which would only ask again sooner than it said.
func TestDownloadGivesUpOnARegistryThatKeepsSayingSlowDown(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	s.misbehave = func(_ int, _ blobRequest, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	var waits waitLog
	d := s.downloaderFor(t, 1, 0)
	d.Tune = instantWaits(&waits)
	_, restarts, err := s.fetch(t, d)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the 503", err)
	}
	if got := len(s.seen()); got != throttleAttempts {
		t.Fatalf("%d requests, want %d", got, throttleAttempts)
	}
	if restarts != 0 {
		t.Fatalf("the blob was restarted %d times after a throttled answer", restarts)
	}
	got := waits.all()
	if len(got) != throttleAttempts-1 {
		t.Fatalf("waited %d times (%v), want %d: one before each request after the first", len(got), got, throttleAttempts-1)
	}
	// The answers arrive without any real time passing between them (the waits
	// are instant), so they are one burst to the budget and share one pause; the
	// doubling of the pause between separate events is pinned in connbudget.
	for _, d := range got {
		if d < time.Second || d > 30*time.Second {
			t.Fatalf("a pause of %v after a 503 that named no time: %v", d, got)
		}
	}
	if got[0] <= 0 {
		t.Fatalf("the first retry did not wait: %v", got)
	}
}

// A body that ends early without an error is a broken connection too: it must
// not be written off as a complete (short) blob.
func TestDownloadTreatsAShortBodyAsBroken(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	var cut atomic.Bool
	s.misbehave = func(_ int, _ blobRequest, w http.ResponseWriter, _ *http.Request) bool {
		if cut.Swap(true) {
			return false
		}
		// No Content-Length: the body is chunked and ends cleanly after 10 KiB,
		// which looks like a complete response unless the length is checked.
		w.Write(s.data[:10<<10])
		return true
	}
	reported, _, err := s.fetch(t, s.downloaderFor(t, 1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if reported != s.desc.Size {
		t.Fatalf("reported %d of %d", reported, s.desc.Size)
	}
}

func TestDownloadStopsWhenCancelled(t *testing.T) {
	quickRecovery(t)
	s := newBlobServer(t, 1<<20)
	s.misbehave = func(_ int, _ blobRequest, w http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	err := s.downloaderFor(t, 1, 0).FetchAll(ctx, s.ref, []Descriptor{s.desc}, nil, nil)
	if err == nil {
		t.Fatal("a cancelled download reported success")
	}
}

// The crawl floor is judged per connection, and a pull runs up to
// DefaultMaxConnections at once; their floors added up must stay below what a
// merely slow link delivers, or every connection on such a node is recycled
// every window (the budget's quota keeps it from being all of them at once,
// but a floor that high would still churn half of them).
func TestSlowMinRateStaysBelowASlowLinkAcrossAllConnections(t *testing.T) {
	const slowLink = 400 << 10 // bytes/s: mainland to an overseas CDN at peak
	if total := slowMinRate * int64(DefaultMaxConnections); total > slowLink {
		t.Fatalf("slowMinRate %d B/s x %d connections = %d B/s, above a slow link's %d B/s", slowMinRate, DefaultMaxConnections, total, slowLink)
	}
}
