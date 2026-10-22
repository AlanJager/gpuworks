package apiclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"
)

func TestCloudRequestTimeoutAllowsRetryWithinOperationBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithRequestTimeout(2*time.Second), WithOperationTimeout(10*time.Second), WithReadRetries(1), WithRetryBackoff(time.Second, time.Second))
		calls, start := 0, time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			return cloudResponse(r, 200, `{}`), nil
		})
		var out any
		if err := c.Get(t.Context(), "/hosts", &out); err != nil || calls != 2 || time.Since(start) != 3*time.Second {
			t.Fatalf("request timeout consumed whole operation: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestCloudOperationDeadlineIncludesAllRetryAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithRequestTimeout(2*time.Second), WithOperationTimeout(4*time.Second), WithReadRetries(10), WithRetryBackoff(time.Second, time.Second))
		calls, start := 0, time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		var out any
		err := c.Get(t.Context(), "/hosts", &out)
		clientError(t, err, ErrorTimeout)
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(start) != 4*time.Second {
			t.Fatalf("retry renewed total budget: calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
	})
}

func TestCloudCallerDeadlineAndCancellationTakePrecedence(t *testing.T) {
	for _, beforeSend := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(10))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				cancel()
				return nil, io.EOF
			})
			if beforeSend {
				cancel()
			}
			var out any
			err := c.Get(ctx, "/hosts", &out)
			clientError(t, err, ErrorCanceled)
			want := 1
			if beforeSend {
				want = 0
			}
			if !errors.Is(err, context.Canceled) || calls != want {
				t.Fatalf("canceled query continued: calls=%d err=%v", calls, err)
			}
		})
	}
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithOperationTimeout(time.Hour), WithRequestTimeout(time.Hour))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		start := time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })
		var out any
		if err := c.Get(ctx, "/hosts", &out); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
			t.Fatalf("client extended caller deadline: elapsed=%v err=%v", time.Since(start), err)
		}
	})
}

func TestCloudCancellationInterruptsRetryBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(2), WithRetryBackoff(time.Hour, time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) { calls++; return nil, io.EOF })
		finished := make(chan error, 1)
		go func() { var out any; finished <- c.Get(ctx, "/hosts", &out) }()
		synctest.Wait()
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("cancellation waited for backoff: calls=%d err=%v", calls, err)
		}
	})
}

func TestCloudRequestTimeoutIncludesResponseBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithRequestTimeout(time.Second), WithOperationTimeout(time.Hour))
		closed, start := false, time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			response := cloudResponse(r, 200, "")
			response.Body = &cloudTestBody{Reader: cloudWaitingReader{ctx: r.Context()}, close: func() { closed = true }}
			return response, nil
		})
		var out any
		err := c.Get(t.Context(), "/hosts", &out)
		clientError(t, err, ErrorTimeout)
		if !errors.Is(err, context.DeadlineExceeded) || !closed || time.Since(start) != time.Second {
			t.Fatalf("slow response escaped request timeout: closed=%v elapsed=%v err=%v", closed, time.Since(start), err)
		}
	})
}

type cloudWaitingReader struct{ ctx context.Context }

func (r cloudWaitingReader) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }

func TestCloudCallTimeoutOverrideDoesNotAffectFollowingCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithRequestTimeout(10*time.Second), WithOperationTimeout(20*time.Second))
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/zstack/v1/short" {
				<-r.Context().Done()
				return nil, r.Context().Err()
			}
			select {
			case <-time.After(3 * time.Second):
				return cloudResponse(r, 200, `{}`), nil
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		})
		var out any
		start := time.Now()
		err := c.Get(t.Context(), "/short", &out, WithRequestTimeout(time.Second), WithOperationTimeout(2*time.Second))
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
			t.Fatalf("request override ignored: elapsed=%v err=%v", time.Since(start), err)
		}
		if err := c.Get(t.Context(), "/normal", &out); err != nil || time.Since(start) != 4*time.Second {
			t.Fatalf("override changed following call: elapsed=%v err=%v", time.Since(start), err)
		}
	})
}
