package modelfetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/modelrepo"
)

// A listing uses the real provider HTTP adapter; after recovery, the normal
// resolve/range download must still produce all repository bytes.
func TestListingRecoversTransientFailuresAndDownloads(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			waits := sleeps(t)
			files := modelFiles()
			h := newHub(t, files)
			var listings atomic.Int32
			h.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/api/models/") && listings.Add(1) <= 2 {
					http.Error(w, "temporary upstream failure", status)
					return
				}
				h.serve(w, r)
			})
			f := newFetcher(t, nil, &run{}, h.URL)
			f.cfg.Retry.InitialBackoff, f.cfg.Retry.MaxBackoff = time.Second, 2*time.Second
			dir := t.TempDir()
			if err := fetchAll(t, f, commit, dir); err != nil {
				t.Fatalf("transient listing ended download: %v", err)
			}
			assertFiles(t, dir, files)
			if len(*waits) != 2 || (*waits)[0] != time.Second || (*waits)[1] != 2*time.Second {
				t.Fatalf("retry backoff = %v", *waits)
			}
		})
	}
}

func TestListingTransportRetryIsBoundedAndRetainsCause(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"deadline", context.DeadlineExceeded},
		{"dial timeout", timeoutError{}},
		{"temporary DNS", &net.DNSError{Err: "temporary resolver failure", Name: "hub.example", IsTemporary: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := sleeps(t)
			f := newFetcher(t, nil, &run{}, "https://hub.example")
			var attempts int
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return nil, tc.err
			})}
			f.cfg.Client = func(string) (*http.Client, error) { return client, nil }
			_, err := f.List(context.Background(), commit)
			if attempts != 3 || len(*waits) != 2 || !errors.Is(err, tc.err) || ListingErrorCode(err) != proto.ErrorModelDownloadNetworkExhausted {
				t.Fatalf("attempts=%d waits=%v error=%v; want three attempts and wrapped cause", attempts, *waits, err)
			}
		})
	}
}

func TestListingPermanentHTTPFailureIsNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			waits := sleeps(t)
			h := newHub(t, modelFiles())
			h.listStatus = code
			f := newFetcher(t, nil, &run{}, h.URL)
			_, err := f.List(context.Background(), commit)
			var status *modelrepo.StatusError
			if len(*waits) != 0 || !errors.As(err, &status) || status.Code != code {
				t.Fatalf("permanent response lost classification or retried: waits=%v error=%v", *waits, err)
			}
		})
	}
}

func TestListingCancellationStopsBackoff(t *testing.T) {
	f := newFetcher(t, nil, &run{}, "https://hub.example")
	f.cfg.Retry.InitialBackoff = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var attempts int
	f.cfg.Client = func(string) (*http.Client, error) {
		attempts++
		return nil, context.DeadlineExceeded
	}
	f.cfg.OnRetry = func(_ string, _, _ int, _ time.Duration, _ error) { cancel() }
	_, err := f.List(ctx, commit)
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("cancellation: attempts=%d error=%v", attempts, err)
	}
}

func TestListingProviderEnvelopeHasBoundedTransientAndPermanentSemantics(t *testing.T) {
	for _, code := range []int{500, 404, 401} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			waits := sleeps(t)
			var attempts atomic.Int32
			hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if r.URL.Path != "/api/v1/models/"+repo+"/repo/files" || r.URL.Query().Get("Revision") != commit {
					t.Errorf("unexpected ModelScope listing %s", r.URL)
				}
				// ModelScope can carry an error code in an HTTP 200 envelope.
				_, _ = fmt.Fprintf(w, `{"Code":%d,"Message":"provider error","Data":null}`, code)
			}))
			defer hub.Close()
			f := newFetcher(t, nil, &run{}, hub.URL)
			f.cfg.Source = modelrepo.SourceMS
			f.cfg.Retry.LastAttempts = 4
			_, err := f.List(context.Background(), commit)
			want := 1
			if code == 500 {
				want = 4
			}
			var status *modelrepo.StatusError
			if int(attempts.Load()) != want || len(*waits) != want-1 || !errors.As(err, &status) || status.Code != code {
				t.Fatalf("provider Code=%d: attempts=%d waits=%v error=%v", code, attempts.Load(), *waits, err)
			}
			wantCode := map[int]string{500: proto.ErrorModelDownloadNetworkExhausted, 404: proto.ErrorModelDownloadSourceNotFound, 401: proto.ErrorModelDownloadSourceAccessDenied}[code]
			if got := ListingErrorCode(err); got != wantCode {
				t.Fatalf("provider Code=%d produced outcome %q want %q", code, got, wantCode)
			}
		})
	}
}

func TestListingServiceUnavailablePreservesThrottleWaitAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cancel   bool
		recover  bool
		attempts int
	}{
		{"recovers", false, true, 2},
		{"exhausts", false, false, 3},
		{"cancels", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := sleeps(t)
			h := newHub(t, modelFiles())
			var attempts atomic.Int32
			h.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 || !tc.recover {
					w.Header().Set("Retry-After", "3600")
					http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				h.serve(w, r)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFetcher(t, nil, &run{}, h.URL)
			f.cfg.OnRetry = func(_ string, _, _ int, _ time.Duration, _ error) {
				if tc.cancel {
					cancel()
				}
			}
			_, err := f.List(ctx, commit)
			if int(attempts.Load()) != tc.attempts || len(*waits) == 0 {
				t.Fatalf("attempts=%d waits=%v err=%v", attempts.Load(), *waits, err)
			}
			for _, wait := range *waits {
				if wait != MaxThrottleWait {
					t.Fatalf("503 wait=%v want capped Retry-After=%v", wait, MaxThrottleWait)
				}
			}
			if tc.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation=%v", err)
				}
			} else if tc.recover {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var status *modelrepo.StatusError
				if !errors.As(err, &status) || status.Code != http.StatusServiceUnavailable {
					t.Fatalf("exhaustion=%v", err)
				}
			}
		})
	}
}
