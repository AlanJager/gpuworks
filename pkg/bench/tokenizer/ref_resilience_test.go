package tokenizer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const validTokenizer = `{"model":{"vocab":{"hello":0,"world":1}}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func tokenizerClient(t *testing.T, f roundTripFunc) {
	t.Helper()
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: f}
	t.Cleanup(func() { http.DefaultClient = old })
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}
func tokenizerResponse(body string) *http.Response {
	return &http.Response{StatusCode: 200, Status: "200 OK", Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestLoadRefRetriesTransientDNSAndUsesOfflineCache(t *testing.T) {
	attempts := 0
	tokenizerClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.DNSError{Err: "temporary failure in name resolution", Name: r.URL.Hostname(), IsTemporary: true}
		}
		return tokenizerResponse(validTokenizer), nil
	})
	ref := "https://tokenizer.invalid/model/tokenizer.json"
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d, want 2", attempts)
	}
	http.DefaultClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("cache hit accessed network")
		return nil, errors.New("offline")
	})
	if tok, err := LoadRef(context.Background(), ref); err != nil || tok.Count("hello") != 1 {
		t.Fatalf("cached tokenizer=%v error=%v", tok, err)
	}
}
func TestLoadRefRetryBoundAndCancellation(t *testing.T) {
	attempts := 0
	tokenizerClient(t, func(r *http.Request) (*http.Response, error) {
		attempts++
		return nil, &net.DNSError{Err: "server misbehaving", Name: r.URL.Hostname(), IsTemporary: true}
	})
	if _, err := LoadRef(context.Background(), "https://tokenizer.invalid/bounded"); err == nil {
		t.Fatal("expected exhausted DNS error")
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d, want 3", attempts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempts = 0
	http.DefaultClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		cancel()
		return nil, &net.DNSError{Err: "temporary", IsTemporary: true}
	})
	if _, err := LoadRef(ctx, "https://tokenizer.invalid/canceled"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts after cancellation=%d", attempts)
	}
}
func TestLoadRefPermanentAndInvalidFailuresAreNotCached(t *testing.T) {
	for _, kind := range []string{"nxdomain", "404", "invalid", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			attempts := 0
			tokenizerClient(t, func(*http.Request) (*http.Response, error) {
				attempts++
				switch kind {
				case "nxdomain":
					return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
				case "404":
					return &http.Response{StatusCode: 404, Status: "404 Not Found", Body: io.NopCloser(strings.NewReader("missing")), Header: make(http.Header)}, nil
				case "truncated":
					return tokenizerResponse(`{"model":`), nil
				default:
					return tokenizerResponse(`{"model":{"vocab":{}}}`), nil
				}
			})
			ref := "https://tokenizer.invalid/" + kind
			if _, err := LoadRef(context.Background(), ref); err == nil {
				t.Fatal("expected error")
			}
			if attempts != 1 {
				t.Fatalf("attempts=%d", attempts)
			}
			http.DefaultClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { attempts++; return tokenizerResponse(validTokenizer), nil })
			if _, err := LoadRef(context.Background(), ref); err != nil {
				t.Fatal(err)
			}
			if attempts != 2 {
				t.Fatalf("failed download poisoned cache: attempts=%d", attempts)
			}
		})
	}
}
func TestLoadRefIgnoresExpiredAndCorruptCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = io.WriteString(w, validTokenizer) }))
	defer srv.Close()
	ref := srv.URL + "/tokenizer.json"
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_CACHE_HOME"), "gpuworks", "tokenizers", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("cache files=%v error=%v", files, err)
	}
	expired := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(files[0], expired, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], []byte(`invalid`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestLoadRefCanceledCacheHitAndReadOnlyCache(t *testing.T) {
	calls := 0
	tokenizerClient(t, func(*http.Request) (*http.Response, error) { calls++; return tokenizerResponse(validTokenizer), nil })
	ref := "https://tokenizer.invalid/cancel-cache"
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadRef(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache load=%v", err)
	}
	if calls != 1 {
		t.Fatalf("canceled load accessed network: %d", calls)
	}
	// A file at the cache root faithfully makes MkdirAll fail, even as root.
	root := filepath.Join(t.TempDir(), "cache-file")
	if err := os.WriteFile(root, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", root)
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatalf("cache failure broke download: %v", err)
	}
}

func TestLoadRefRetriesServerFailuresButNotRetryAfter(t *testing.T) {
	for _, retryAfter := range []string{"", "120"} {
		t.Run("retry_after_"+retryAfter, func(t *testing.T) {
			attempts := 0
			tokenizerClient(t, func(*http.Request) (*http.Response, error) {
				attempts++
				if attempts == 1 {
					return &http.Response{StatusCode: 503, Status: "503 Service Unavailable", Header: http.Header{"Retry-After": []string{retryAfter}}, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
				}
				return tokenizerResponse(validTokenizer), nil
			})
			_, err := LoadRef(context.Background(), "https://tokenizer.invalid/503")
			if retryAfter == "" && (err != nil || attempts != 2) {
				t.Fatalf("attempts=%d error=%v", attempts, err)
			}
			if retryAfter != "" && (err == nil || attempts != 1) {
				t.Fatalf("attempts=%d error=%v", attempts, err)
			}
		})
	}
}

func TestLoadRefCancellationInterruptsDownload(t *testing.T) {
	tokenizerClient(t, func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := LoadRef(ctx, "https://tokenizer.invalid/slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("download deadline=%v", err)
	}
}

func TestLoadRefCancellationInterruptsBackoff(t *testing.T) {
	attempts := 0
	tokenizerClient(t, func(*http.Request) (*http.Response, error) {
		attempts++
		return nil, &net.DNSError{Err: "temporary", IsTemporary: true}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := LoadRef(ctx, "https://tokenizer.invalid/backoff"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backoff deadline=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("retried after deadline: attempts=%d", attempts)
	}
}

func TestExpiredCacheCannotMaskPermanentDownloadFailure(t *testing.T) {
	tokenizerClient(t, func(*http.Request) (*http.Response, error) { return tokenizerResponse(validTokenizer), nil })
	ref := "https://tokenizer.invalid/expired"
	if _, err := LoadRef(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(tokenizerCachePath(ref), expired, expired); err != nil {
		t.Fatal(err)
	}
	http.DefaultClient.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Status: "403 Forbidden", Body: io.NopCloser(strings.NewReader("forbidden")), Header: make(http.Header)}, nil
	})
	if _, err := LoadRef(context.Background(), ref); !errors.Is(err, ErrDownload) || !strings.Contains(err.Error(), "403") {
		t.Fatalf("stale tokenizer masked permanent failure: %v", err)
	}
}

func TestTokenizerCacheSeparatesSourcesRevisionsAndCredentials(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	first := tokenizerCachePath("https://hub.invalid/org/model/main/tokenizer.json?token=first")
	for _, ref := range []string{"https://mirror.invalid/org/model/main/tokenizer.json?token=first", "https://hub.invalid/org/model/revision/tokenizer.json?token=first", "https://hub.invalid/org/model/main/tokenizer.json?token=second"} {
		if tokenizerCachePath(ref) == first {
			t.Fatal("distinct tokenizer source reused cache key")
		}
	}
	if strings.Contains(first, "first") || strings.Contains(first, "hub.invalid") {
		t.Fatal("cache path exposed credentials/source")
	}
}

func TestTokenizerConcurrentCacheWritesStayComplete(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := tokenizerCachePath("https://tokenizer.invalid/concurrent")
	var writers sync.WaitGroup
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() { defer writers.Done(); cacheTokenizer(path, []byte(validTokenizer)) }()
	}
	writers.Wait()
	if tok := cachedTokenizer(path); tok == nil || tok.Count("hello") != 1 {
		t.Fatal("concurrent writers left corrupt cache")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tokenizer-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary cache files leaked: %v error=%v", files, err)
	}
}

func TestLoadRefRetriesInterruptedHTTPBody(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, `{"model":`)
			return
		}
		_, _ = io.WriteString(w, validTokenizer)
	}))
	defer srv.Close()
	if _, err := LoadRef(context.Background(), srv.URL+"/tokenizer.json"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("body interruption attempts=%d", calls)
	}
}

func TestLoadRefHonorsShortRetryAfterAndPastDate(t *testing.T) {
	for _, hint := range []string{"1", "0", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(hint, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			calls := 0
			var firstAt time.Time
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					firstAt = time.Now()
					w.Header().Set("Retry-After", hint)
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				if hint == "1" && time.Since(firstAt) < time.Second {
					t.Error("retried before Retry-After elapsed")
				}
				_, _ = io.WriteString(w, validTokenizer)
			}))
			defer srv.Close()
			if _, err := LoadRef(context.Background(), srv.URL+"/tokenizer.json"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestLoadRefRetryAfterBudgetAndCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWait), func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				hint := "120"
				if cancelWait {
					hint = "1"
				}
				w.Header().Set("Retry-After", hint)
				http.Error(w, "temporarily unavailable", http.StatusTooManyRequests)
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelWait {
				timer := time.AfterFunc(20*time.Millisecond, cancel)
				defer timer.Stop()
			}
			_, err := LoadRef(ctx, srv.URL+"/tokenizer.json")
			if cancelWait && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error=%v", err)
			}
			if !cancelWait && !errors.Is(err, ErrDownload) {
				t.Fatalf("budget error=%v", err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestTokenizerRetryAfterParsesFutureDateAndOverflow(t *testing.T) {
	future := time.Now().Add(10 * time.Second).UTC().Format(http.TimeFormat)
	if delay := tokenizerRetryAfter(future); delay < 8*time.Second || delay > 10*time.Second {
		t.Fatalf("HTTP-date wait=%s", delay)
	}
	if delay := tokenizerRetryAfter("9999999999999999999999999999"); delay != 30*time.Second {
		t.Fatalf("overflow wait=%s", delay)
	}
}
