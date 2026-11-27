package modelfetch

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// Zero means an accepted request that never responds before its client
// deadline; the last entry repeats. Other entries are real HTTP statuses.
func listingTimeoutHub(t *testing.T, replies ...int) (*hub, *atomic.Int32) {
	t.Helper()
	h := newHub(t, modelFiles())
	var listings atomic.Int32
	h.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/models/") {
			h.serve(w, r)
			return
		}
		index := int(listings.Add(1)) - 1
		status := replies[min(index, len(replies)-1)]
		switch status {
		case 0:
			<-r.Context().Done()
		case http.StatusOK:
			h.serve(w, r)
		default:
			if status == http.StatusServiceUnavailable {
				w.Header().Set("Retry-After", "2")
			}
			http.Error(w, "fixture upstream failure", status)
		}
	})
	return h, &listings
}

func timeoutFetcher(t *testing.T, run *run, first *hub, rest ...string) *Fetcher {
	t.Helper()
	f := newFetcher(t, nil, run, append([]string{first.URL}, rest...)...)
	f.cfg.Retry = RetryPolicy{MaxAttempts: 6, LastAttempts: 16}
	client := &http.Client{Timeout: 100 * time.Millisecond}
	f.cfg.Client = func(base string) (*http.Client, error) {
		if base == first.URL {
			return client, nil
		}
		return nil, nil
	}
	return f
}

func TestListingTimeoutFallsBackWithoutConsumingTheWholeRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name          string
		replies       []int
		firstAttempts int
		switches      int
	}{
		{"repeated timeouts", []int{0}, 2, 1},
		{"recovers on retry", []int{0, 200}, 2, 0},
		{"503 resets timeout streak", []int{0, 503, 0, 200}, 4, 0},
		{"other transient status keeps budget", []int{502}, 6, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waits := sleeps(t)
			first, attempts := listingTimeoutHub(t, tc.replies...)
			next, nextAttempts := listingTimeoutHub(t, 200)
			r := &run{}
			f := timeoutFetcher(t, r, first, next.URL)
			dir := t.TempDir()
			if err := fetchAll(t, f, commit, dir); err != nil {
				t.Fatal(err)
			}
			assertFiles(t, dir, modelFiles())
			if int(attempts.Load()) != tc.firstAttempts || len(r.switches) != tc.switches || int(nextAttempts.Load()) != tc.switches {
				t.Fatalf("first listing attempts=%d next=%d switches=%+v; want first=%d switches=%d", attempts.Load(), nextAttempts.Load(), r.switches, tc.firstAttempts, tc.switches)
			}
			if tc.name == "503 resets timeout streak" {
				if len(*waits) != 3 || (*waits)[1] != 2*time.Second {
					t.Fatalf("503 lost Retry-After semantics: %v", *waits)
				}
			}
		})
	}
}

func TestListingTimeoutLastSourceKeepsItsAttemptBudget(t *testing.T) {
	sleeps(t)
	h, attempts := listingTimeoutHub(t, 0)
	r := &run{}
	f := timeoutFetcher(t, r, h)
	_, err := f.List(context.Background(), commit)
	if attempts.Load() != 16 || len(r.switches) != 0 || ListingErrorCode(err) != proto.ErrorModelDownloadNetworkExhausted {
		t.Fatalf("last source lost patience: attempts=%d switches=%+v error=%v", attempts.Load(), r.switches, err)
	}
}

func TestListingTimeoutCancellationDoesNotAdvanceSources(t *testing.T) {
	sleeps(t)
	h, attempts := listingTimeoutHub(t, 0)
	next, nextAttempts := listingTimeoutHub(t, 200)
	r := &run{}
	f := timeoutFetcher(t, r, h, next.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.cfg.OnRetry = func(_ string, _, _ int, _ time.Duration, _ error) { cancel() }
	_, err := f.List(ctx, commit)
	if !errors.Is(err, context.Canceled) || attempts.Load() != 1 || nextAttempts.Load() != 0 || len(r.switches) != 0 || ListingErrorCode(err) != "" {
		t.Fatalf("cancellation moved to another source: attempts=%d next=%d switches=%+v error=%v", attempts.Load(), nextAttempts.Load(), r.switches, err)
	}
}
