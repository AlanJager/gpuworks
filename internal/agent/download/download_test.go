package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pattern builds a deterministic byte pattern of length n.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + 7) % 251)
	}
	return b
}

func sum(b []byte) [32]byte { return sha256.Sum256(b) }

// rangeServer serves data honoring Range requests (via http.ServeContent).
func rangeServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "model.bin", time.Time{}, bytes.NewReader(data))
	}))
}

// noRangeServer ignores Range and always returns the full 200 body.
func noRangeServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}))
}

func TestMultiConnectionRange(t *testing.T) {
	data := pattern(512 * 1024)
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	err := Download(context.Background(), Options{
		URL:         srv.URL,
		Dest:        dest,
		Concurrency: 5,
	}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(data) {
		t.Fatalf("content mismatch: got %d bytes", len(got))
	}
	assertCleanedUp(t, dest)
}

func TestMultiConnectionRejectsCleanTruncatedRange(t *testing.T) {
	data := pattern(128 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedRange(t, r)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		if start == 0 && end == 0 { // range-support probe
			_, _ = w.Write(data[:1])
			return
		}
		// Flush the headers before the short body so net/http uses chunked
		// framing and the client observes a clean EOF rather than relying on a
		// mismatched Content-Length to synthesize ErrUnexpectedEOF.
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		partialEnd := start + (end-start+1)/2
		_, _ = w.Write(data[start:partialEnd])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "truncated.bin")
	err := Download(context.Background(), Options{URL: srv.URL, Dest: dest, Concurrency: 2}, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated clean EOF error=%v, want io.ErrUnexpectedEOF", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatalf("truncated download produced final file: %v", statErr)
	}
}

func TestFirstErrPrefersRootTransportErrorOverWorkerCancellation(t *testing.T) {
	errCh := make(chan error, 2)
	errCh <- context.Canceled
	rootErr := fmt.Errorf("download chunk: %w", io.ErrUnexpectedEOF)
	errCh <- rootErr
	close(errCh)

	err := firstErr(errCh)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("firstErr=%v, want root io.ErrUnexpectedEOF", err)
	}
}

func TestMultiConnectionRejectsMismatchedContentRange(t *testing.T) {
	data := pattern(64 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestedRange(t, r)
		if start != 0 || end != 0 {
			start++
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[:1])
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "wrong-range.bin")
	err := Download(context.Background(), Options{URL: srv.URL, Dest: dest, Concurrency: 2}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched Content-Range error=%v", err)
	}
}

func requestedRange(t *testing.T, r *http.Request) (int64, int64) {
	t.Helper()
	raw := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	startText, endText, ok := strings.Cut(raw, "-")
	if !ok {
		t.Fatalf("invalid fixture Range header %q", r.Header.Get("Range"))
	}
	start, err := strconv.ParseInt(startText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	end, err := strconv.ParseInt(endText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return start, end
}

func TestSingleConnectionFallback(t *testing.T) {
	data := pattern(200 * 1024)
	srv := noRangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	err := Download(context.Background(), Options{
		URL:         srv.URL,
		Dest:        dest,
		Concurrency: 4, // should fall back to single since server ignores Range
	}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(data) {
		t.Fatalf("content mismatch")
	}
	assertCleanedUp(t, dest)
}

func TestResumeSingleConnection(t *testing.T) {
	data := pattern(256 * 1024)
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	partPath := dest + ".part"

	// Pre-create a partial .part with the first 100 KiB.
	prefix := 100 * 1024
	if err := os.WriteFile(partPath, data[:prefix], 0o644); err != nil {
		t.Fatal(err)
	}

	err := Download(context.Background(), Options{
		URL:         srv.URL,
		Dest:        dest,
		Concurrency: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(data) {
		t.Fatalf("resumed content mismatch, len=%d", len(got))
	}
	assertCleanedUp(t, dest)
}

func TestResumeMultiConnectionWithState(t *testing.T) {
	size := 400 * 1024
	data := pattern(size)
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	partPath := dest + ".part"
	statePath := dest + ".part.state"

	// With Concurrency=2 the split is [0,mid-1] and [mid,size-1].
	mid := int64(size / 2)

	// Pre-create a full-size .part with the first chunk already present.
	buf := make([]byte, size)
	copy(buf[:mid], data[:mid])
	if err := os.WriteFile(partPath, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	// Sidecar marks chunk 0 complete (done == end+1 == mid) and chunk 1 untouched.
	writeState(statePath, downloadState{
		URL:    srv.URL,
		Size:   int64(size),
		Chunks: []int64{mid, mid},
	})

	err := Download(context.Background(), Options{
		URL:         srv.URL,
		Dest:        dest,
		Concurrency: 2,
	}, nil)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(data) {
		t.Fatalf("resumed multi content mismatch, len=%d", len(got))
	}
	assertCleanedUp(t, dest)
}

func TestRateLimit(t *testing.T) {
	data := pattern(48 * 1024) // 48 KiB
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	start := time.Now()
	err := Download(context.Background(), Options{
		URL:          srv.URL,
		Dest:         dest,
		Concurrency:  1,
		RateLimitKBs: 64, // 64 KiB/s -> ~0.75s for 48 KiB starting from empty
	}, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Fatalf("rate limit too fast: %v", elapsed)
	}
	// Keep the upper bound generous enough for a loaded CI runner. The
	// correctness assertion is the lower bound (the transfer must be throttled);
	// scheduler and container startup pauses can add several seconds without
	// indicating a limiter regression.
	if elapsed > 8*time.Second {
		t.Fatalf("rate limit unexpectedly slow: %v", elapsed)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if sum(got) != sum(data) {
		t.Fatalf("rate-limited content mismatch")
	}
}

func TestContextCancel(t *testing.T) {
	data := pattern(1024 * 1024)
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := Download(ctx, Options{
		URL:          srv.URL,
		Dest:         dest,
		Concurrency:  4,
		RateLimitKBs: 8, // 8 KiB/s -> download can't finish before cancel
	}, nil)
	if err == nil {
		t.Fatal("expected error from cancellation")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if _, statErr := os.Stat(dest); statErr == nil {
		t.Fatal("dest should not exist after cancellation")
	}
}

func TestProgressCallback(t *testing.T) {
	data := pattern(256 * 1024)
	srv := rangeServer(t, data)
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "model.bin")
	var calls int64
	var lastDone int64
	var lastTotal int64
	err := Download(context.Background(), Options{
		URL:          srv.URL,
		Dest:         dest,
		Concurrency:  2,
		RateLimitKBs: 256, // slow it down so multiple ticks happen
	}, func(p Progress) {
		atomic.AddInt64(&calls, 1)
		atomic.StoreInt64(&lastDone, p.Done)
		atomic.StoreInt64(&lastTotal, p.Total)
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if atomic.LoadInt64(&calls) == 0 {
		t.Fatal("expected at least one progress callback")
	}
	if got := atomic.LoadInt64(&lastDone); got != int64(len(data)) {
		t.Fatalf("final Done = %d, want %d", got, len(data))
	}
	if got := atomic.LoadInt64(&lastTotal); got != int64(len(data)) {
		t.Fatalf("Total = %d, want %d", got, len(data))
	}
}

// assertCleanedUp verifies the .part and .state sidecars are gone after success.
func assertCleanedUp(t *testing.T, dest string) {
	t.Helper()
	if _, err := os.Stat(dest + ".part"); err == nil {
		t.Errorf(".part file not cleaned up")
	}
	if _, err := os.Stat(dest + ".part.state"); err == nil {
		t.Errorf(".part.state file not cleaned up")
	}
}

// A file fetched from a second address continues the bytes the first one
// delivered when both downloads name the same bytes with one ResumeKey, and
// starts over when they do not.
func TestResumeKeyCarriesProgressAcrossAddresses(t *testing.T) {
	size := 400 * 1024
	data := pattern(size)
	mid := int64(size / 2)
	for name, tc := range map[string]struct {
		key        string
		wantRanged int64 // bytes the second address had to send
	}{
		"same bytes": {"huggingface/org/model@0123abc/model.bin", int64(size) - mid},
		"no key":     {"", int64(size)},
	} {
		t.Run(name, func(t *testing.T) {
			var sent atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-0" {
					if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
						from, to, _ := strings.Cut(spec, "-")
						start, _ := strconv.ParseInt(from, 10, 64)
						end, _ := strconv.ParseInt(to, 10, 64)
						sent.Add(end - start + 1)
					}
				}
				http.ServeContent(w, r, "model.bin", time.Time{}, bytes.NewReader(data))
			}))
			defer srv.Close()
			dest := filepath.Join(t.TempDir(), "model.bin")
			buf := make([]byte, size)
			copy(buf[:mid], data[:mid])
			if err := os.WriteFile(dest+".part", buf, 0o644); err != nil {
				t.Fatal(err)
			}
			first := tc.key
			if first == "" {
				first = "https://hf-mirror.com/org/model/resolve/0123abc/model.bin"
			}
			writeState(dest+".part.state", downloadState{URL: first, Size: int64(size), Chunks: []int64{mid, mid}})

			if err := Download(context.Background(), Options{URL: srv.URL + "/org/model/resolve/0123abc/model.bin", Dest: dest, Concurrency: 2, ResumeKey: tc.key}, nil); err != nil {
				t.Fatalf("Download: %v", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil || sum(got) != sum(data) {
				t.Fatalf("content mismatch: %v", err)
			}
			if sent.Load() != tc.wantRanged {
				t.Fatalf("second address sent %d bytes, want %d", sent.Load(), tc.wantRanged)
			}
		})
	}
}

// A status that cannot be the file is a *StatusError carrying the code, so a
// caller can tell an address that lacks the file from a broken transfer.
func TestErrorStatusesAreTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
	}))
	defer srv.Close()
	err := Download(context.Background(), Options{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "model.bin")}, nil)
	var status *StatusError
	if !errors.As(err, &status) || status.Code != http.StatusBadGateway || !strings.Contains(err.Error(), "unexpected probe status 502") {
		t.Fatalf("err = %v, want a StatusError with code 502", err)
	}
}
