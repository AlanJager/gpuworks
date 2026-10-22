package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := stallTimeout
	stallTimeout = d
	t.Cleanup(func() { stallTimeout = old })
}

// pacedServer serves data in 100-byte pieces, one every pace, honoring Range
// (ranged or not). A request for more than one piece therefore takes longer
// than a short client timeout while bytes keep flowing. After stallAfter bytes
// of a response it stops sending and blocks (0 = never); headerDelay holds back
// the response headers.
func pacedServer(t *testing.T, data []byte, pace time.Duration, stallAfter int, headerDelay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := 0, len(data)-1
		ranged := false
		if spec, ok := strings.CutPrefix(r.Header.Get("Range"), "bytes="); ok {
			ranged = true
			from, to, _ := strings.Cut(spec, "-")
			start, _ = strconv.Atoi(from)
			if to != "" {
				end, _ = strconv.Atoi(to)
			}
		}
		if headerDelay > 0 {
			select {
			case <-time.After(headerDelay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		if ranged {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		sent := 0
		for off := start; off <= end; off += 100 {
			if stallAfter > 0 && sent >= stallAfter && end > start {
				<-r.Context().Done()
				return
			}
			next := min(off+100, end+1)
			if _, err := w.Write(data[off:next]); err != nil {
				return
			}
			sent += next - off
			w.(http.Flusher).Flush()
			select {
			case <-time.After(pace):
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRequestTimeoutIsFifteenMinutes(t *testing.T) {
	if requestTimeout != 15*time.Minute {
		t.Fatalf("requestTimeout = %s, want 15m", requestTimeout)
	}
	if got := defaultClient().Timeout; got != 15*time.Minute {
		t.Fatalf("default client timeout = %s, want 15m", got)
	}
	short := &http.Client{Timeout: 5 * time.Minute}
	if got := withRequestTimeout(short).Timeout; got != 15*time.Minute {
		t.Fatalf("caller client with a 5m timeout was raised to %s, want 15m", got)
	}
	if short.Timeout != 5*time.Minute {
		t.Fatalf("caller's own client was mutated: %s", short.Timeout)
	}
	if got := withRequestTimeout(&http.Client{Timeout: 48 * time.Hour}).Timeout; got != 48*time.Hour {
		t.Fatalf("a longer timeout must be kept, got %s", got)
	}
	if got := withRequestTimeout(&http.Client{}).Timeout; got != 0 {
		t.Fatalf("a client without a timeout must stay without one, got %s", got)
	}
}

// A request that takes longer than the caller's client timeout still completes
// while bytes keep arriving: only silence, not total duration, ends a transfer.
func TestSlowFlowingDownloadIsNotCutOffByClientTimeout(t *testing.T) {
	setStallTimeout(t, 400*time.Millisecond)
	data := pattern(1200)
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			srv := pacedServer(t, data, 60*time.Millisecond, 0, 0)
			dest := filepath.Join(t.TempDir(), "model.bin")
			err := Download(context.Background(), Options{
				URL: srv.URL, Dest: dest, Concurrency: concurrency,
				Client: &http.Client{Timeout: 100 * time.Millisecond},
			}, nil)
			if err != nil {
				t.Fatalf("download: %v", err)
			}
			got, rerr := os.ReadFile(dest)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !bytes.Equal(got, data) {
				t.Fatalf("content differs after a slow download")
			}
		})
	}
}

func TestStalledBodyIsAbandonedAsTransient(t *testing.T) {
	setStallTimeout(t, 250*time.Millisecond)
	data := pattern(1200)
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			srv := pacedServer(t, data, 10*time.Millisecond, 100, 0)
			started := time.Now()
			err := Download(context.Background(), Options{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "model.bin"), Concurrency: concurrency}, nil)
			if err == nil {
				t.Fatal("a body that stops sending must fail")
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrStalled) {
				t.Fatalf("stall error must read as a stall and a transient timeout, got %v", err)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("stall took %s to detect", elapsed)
			}
		})
	}
}

func TestStalledResponseHeadersAreAbandonedAsTransient(t *testing.T) {
	setStallTimeout(t, 250*time.Millisecond)
	srv := pacedServer(t, pattern(300), time.Millisecond, 0, 30*time.Second)
	started := time.Now()
	err := Download(context.Background(), Options{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "model.bin")}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrStalled) {
		t.Fatalf("a server that never answers must fail as a stall, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("header stall took %s to detect", elapsed)
	}
}

func TestCallerCancelIsNotReportedAsStall(t *testing.T) {
	setStallTimeout(t, time.Minute)
	srv := pacedServer(t, pattern(1200), 200*time.Millisecond, 0, 0)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	err := Download(ctx, Options{URL: srv.URL, Dest: filepath.Join(t.TempDir(), "model.bin"), Concurrency: 1}, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrStalled) {
		t.Fatalf("cancel must surface as context.Canceled, got %v", err)
	}
}
