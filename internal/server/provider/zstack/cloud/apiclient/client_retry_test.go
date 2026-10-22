package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestCloudQueryRetriesTransientFailuresWithBoundedBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(3), WithRetryBackoff(time.Second, 2*time.Second))
		start := time.Now()
		var attempts []time.Duration
		var dates []string
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Since(start))
			dates = append(dates, r.Header.Get("Date"))
			if !validTestAccessKey(r) {
				t.Fatal("retry reused an invalid signature")
			}
			switch len(attempts) {
			case 1:
				return nil, io.ErrUnexpectedEOF
			case 2:
				return cloudResponse(r, 502, "gateway unavailable"), nil
			case 3:
				return cloudResponse(r, 504, "gateway timeout"), nil
			default:
				return cloudResponse(r, 200, `{"value":"recovered"}`), nil
			}
		})
		var out struct {
			Value string `json:"value"`
		}
		if err := c.Get(t.Context(), "/hosts", &out); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(attempts, []time.Duration{0, time.Second, 3 * time.Second, 5 * time.Second}) || out.Value != "recovered" {
			t.Fatalf("attempts=%v response=%+v", attempts, out)
		}
		if dates[0] == dates[1] {
			t.Fatal("retry did not renew its authentication date")
		}
	})
}

func TestCloudQueryRetryLimitCountsAdditionalAttempts(t *testing.T) {
	for _, retries := range []int{0, 1, 3} {
		synctest.Test(t, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(retries), WithRetryBackoff(time.Millisecond, time.Millisecond))
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, io.EOF })
			var out any
			err := c.Get(t.Context(), "/hosts", &out)
			clientError(t, err, ErrorNetwork)
			if calls != retries+1 || !errors.Is(err, io.EOF) {
				t.Fatalf("calls=%d retries=%d err=%v", calls, retries, err)
			}
		})
	}
}

func TestCloudQueryDoesNotRetryPermanentTransportFailure(t *testing.T) {
	c := clientForTest(t, WithReadRetries(3))
	calls := 0
	permanent := errors.New("invalid transport configuration")
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, permanent })
	var out any
	err := c.Get(t.Context(), "/hosts", &out)
	if calls != 1 || !errors.Is(err, permanent) {
		t.Fatalf("permanent failure retried or lost: calls=%d err=%v", calls, err)
	}
}

func TestCloudQueryClosesTruncatedResponseBeforeRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(1), WithRetryBackoff(time.Millisecond, time.Millisecond))
		closed, calls := false, 0
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				response := cloudResponse(r, 200, "")
				response.Body = &cloudTestBody{Reader: cloudBrokenReader{}, close: func() { closed = true }}
				return response, nil
			}
			if !closed {
				t.Fatal("retry started before releasing the previous response")
			}
			return cloudResponse(r, 200, `{}`), nil
		})
		var out any
		if err := c.Get(t.Context(), "/hosts", &out); err != nil || calls != 2 {
			t.Fatalf("retry failed: calls=%d err=%v", calls, err)
		}
	})
}

type cloudBrokenReader struct{}

func (cloudBrokenReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestCloudQueryHonorsRetryAfterWithinOperationDeadline(t *testing.T) {
	for _, form := range []string{"seconds", "date", "exceeds operation", "invalid"} {
		t.Run(form, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := clientForTest(t, WithReadRetries(1), WithOperationTimeout(5*time.Second), WithRetryBackoff(time.Second, time.Second))
				calls, start := 0, time.Now()
				c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						response := cloudResponse(r, 429, "rate limited")
						switch form {
						case "seconds":
							response.Header.Set("Retry-After", "3")
						case "date":
							response.Header.Set("Retry-After", start.Add(3*time.Second).UTC().Format(http.TimeFormat))
						case "exceeds operation":
							response.Header.Set("Retry-After", "9223372036854775807")
						case "invalid":
							response.Header.Set("Retry-After", "not-a-date")
						}
						return response, nil
					}
					return cloudResponse(r, 200, `{}`), nil
				})
				var out any
				err := c.Get(t.Context(), "/hosts", &out)
				if form == "exceeds operation" {
					if !errors.Is(err, context.DeadlineExceeded) || calls != 1 || time.Since(start) != 5*time.Second {
						t.Fatalf("Retry-After exceeded budget: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
					}
					return
				}
				want := 3 * time.Second
				if form == "invalid" {
					want = time.Second
				}
				if err != nil || calls != 2 || time.Since(start) != want {
					t.Fatalf("Retry-After ignored: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
				}
			})
		})
	}
}

func TestCloudCallOverridesDoNotChangeOtherCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(0), WithRetryBackoff(time.Second, time.Second))
		var mu sync.Mutex
		calls := map[string]int{}
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			calls[r.URL.Path]++
			attempt := calls[r.URL.Path]
			mu.Unlock()
			if attempt == 1 {
				return nil, io.EOF
			}
			return cloudResponse(r, 200, `{}`), nil
		})
		results := make(chan error, 2)
		go func() { var out any; results <- c.Get(t.Context(), "/retry", &out, WithReadRetries(1)) }()
		go func() { var out any; results <- c.Get(t.Context(), "/no-retry", &out) }()
		first, second := <-results, <-results
		if (first == nil) == (second == nil) {
			t.Fatalf("per-call policy crossed calls: %v / %v", first, second)
		}
		if calls["/zstack/v1/retry"] != 2 || calls["/zstack/v1/no-retry"] != 1 || c.policy.readRetries != 0 {
			t.Fatalf("per-call policy mutated shared client: %v", calls)
		}
	})
}
