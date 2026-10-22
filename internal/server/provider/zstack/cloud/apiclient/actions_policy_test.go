package apiclient

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"testing"
	"testing/synctest"
	"time"
)

func TestCloudActionRetriesJobQueriesWithoutResubmittingMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(1), WithPollInterval(time.Second), WithRetryBackoff(time.Second, time.Second))
		writes, polls := 0, 0
		start := time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			if !validTestAccessKey(r) {
				t.Fatal("job query lost signed authentication")
			}
			if r.Method == http.MethodPut {
				writes++
				return cloudResponse(r, 202, `{"location":"http://internal.example/zstack/v1/api-jobs/job"}`), nil
			}
			if r.URL.Host != "cloud.example" || r.URL.Path != "/zstack/v1/api-jobs/job" {
				t.Fatalf("job moved endpoint: %s", r.URL)
			}
			polls++
			switch polls {
			case 1, 2:
				return cloudResponse(r, 202, `{}`), nil
			case 3:
				return nil, io.EOF
			default:
				return cloudResponse(r, 200, `{}`), nil
			}
		})
		if err := c.PutAction(t.Context(), "/actions", map[string]any{}); err != nil {
			t.Fatal(err)
		}
		if writes != 1 || polls != 4 || time.Since(start) != 3*time.Second {
			t.Fatalf("polling consumed retries or repeated write: writes=%d polls=%d elapsed=%v", writes, polls, time.Since(start))
		}
	})
}

func TestCloudActionDistinguishesRejectedAndUnknownSubmission(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		err     error
		kind    ErrorKind
		unknown bool
	}{
		{"lost response", 0, "", io.EOF, ErrorNetwork, true},
		{"gateway timeout", 504, "gateway timeout", nil, ErrorNetwork, true},
		{"malformed success", 200, "not-json", nil, ErrorResponse, true},
		{"malformed failure", 503, `{"error":"proxy failure"}`, nil, ErrorResponse, true},
		{"missing job", 202, `{}`, nil, ErrorResponse, true},
		{"bad job URL", 202, `{"location":"/v1/api-jobs/id?secret=value"}`, nil, ErrorResponse, true},
		{"authentication", 401, `{"error":{"code":"AUTH.1000"}}`, nil, ErrorAuthentication, false},
		{"rejected action", 503, `{"error":{"code":"SYS.1000"}}`, nil, ErrorOperation, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(5))
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if test.err != nil {
					return nil, test.err
				}
				return cloudResponse(r, test.status, test.body), nil
			})
			err := c.PutAction(t.Context(), "/actions", map[string]any{})
			apiErr := clientError(t, err, test.kind)
			if calls != 1 || apiErr.OutcomeUnknown != test.unknown {
				t.Fatalf("submission classification/retry: calls=%d unknown=%v err=%v", calls, apiErr.OutcomeUnknown, err)
			}
			if test.err != nil && !errors.Is(err, test.err) {
				t.Fatal("lost transport cause")
			}
		})
	}
}

func TestCloudJobFailurePreservesIdentityAndCertainty(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		body    string
		kind    ErrorKind
		unknown bool
		code    string
	}{
		{"operation failure", 503, `{"error":{"code":"SYS.1000"}}`, ErrorOperation, false, "SYS.1000"},
		{"lost job", 404, `{"error":{"code":"JOB.GONE"}}`, ErrorRequest, true, "JOB.GONE"},
		{"lost authentication", 401, `{"error":{"code":"AUTH.1000"}}`, ErrorAuthentication, true, "AUTH.1000"},
		{"malformed result", 200, `{"incomplete":`, ErrorResponse, true, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := clientForTest(t, WithReadRetries(3))
			calls := 0
			c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method == http.MethodPut {
					return cloudResponse(r, 202, `{"location":"/v1/api-jobs/task-id"}`), nil
				}
				return cloudResponse(r, test.status, test.body), nil
			})
			err := c.PutAction(t.Context(), "/actions", map[string]any{})
			apiErr := clientError(t, err, test.kind)
			if calls != 2 || apiErr.JobID != "task-id" || apiErr.OutcomeUnknown != test.unknown || apiErr.Code != test.code {
				t.Fatalf("job error lost identity or certainty: calls=%d job=%s unknown=%v code=%s err=%v", calls, apiErr.JobID, apiErr.OutcomeUnknown, apiErr.Code, err)
			}
		})
	}
}

func TestCloudJobWaitHasIndependentBudgetAndServerDeadline(t *testing.T) {
	for _, test := range []struct {
		name             string
		requestTimeout   time.Duration
		operationTimeout time.Duration
		serverMillis     int64
		submissionDelay  time.Duration
		want             time.Duration
	}{
		{"longer than HTTP", time.Second, 5 * time.Second, 0, 0, 5 * time.Second},
		{"Cloud deadline", 10 * time.Second, time.Minute, 5000, 2 * time.Second, 5 * time.Second},
		{"untrusted large timeout", time.Second, 3 * time.Second, math.MaxInt64, 0, 3 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := clientForTest(t, WithRequestTimeout(test.requestTimeout), WithOperationTimeout(test.operationTimeout), WithPollInterval(time.Second), WithReadRetries(0))
				start, writes := time.Now(), 0
				c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut {
						writes++
						time.Sleep(test.submissionDelay)
						return cloudResponse(r, 202, `{"location":"/v1/api-jobs/job","apiTimeout":`+strconv.FormatInt(test.serverMillis, 10)+`}`), nil
					}
					return cloudResponse(r, 202, `{}`), nil
				})
				err := c.PutAction(t.Context(), "/actions", map[string]any{})
				apiErr := clientError(t, err, ErrorTimeout)
				if !errors.Is(err, context.DeadlineExceeded) || writes != 1 || apiErr.JobID != "job" || !apiErr.OutcomeUnknown || time.Since(start) != test.want {
					t.Fatalf("job wait budget: elapsed=%v writes=%d job=%s unknown=%v err=%v", time.Since(start), writes, apiErr.JobID, apiErr.OutcomeUnknown, err)
				}
			})
		})
	}
}

func TestCloudActionCancellationRetainsSubmittedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithPollInterval(time.Hour))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		writes, polls := 0, 0
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPut {
				writes++
				return cloudResponse(r, 202, `{"location":"/v1/api-jobs/job"}`), nil
			}
			polls++
			return cloudResponse(r, 202, `{}`), nil
		})
		finished := make(chan error, 1)
		go func() { finished <- c.PutAction(ctx, "/actions", map[string]any{}) }()
		synctest.Wait()
		cancel()
		err := <-finished
		apiErr := clientError(t, err, ErrorCanceled)
		if !errors.Is(err, context.Canceled) || !apiErr.OutcomeUnknown || apiErr.JobID != "job" || writes != 1 || polls != 1 {
			t.Fatalf("cancellation lost outstanding job: writes=%d polls=%d job=%s unknown=%v err=%v", writes, polls, apiErr.JobID, apiErr.OutcomeUnknown, err)
		}
	})
}

func TestCloudServerTimeoutHeaderIsIndependentAndMutationOnly(t *testing.T) {
	c := clientForTest(t, WithServerAPITimeout(1500*time.Millisecond))
	calls := 0
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		want := ""
		if r.Method == http.MethodPut {
			want = "2"
		}
		if r.Header.Get("X-API-Timeout") != want {
			t.Fatalf("server execution timeout header=%q want=%q", r.Header.Get("X-API-Timeout"), want)
		}
		if !validTestAccessKey(r) {
			t.Fatal("server timeout override broke authentication")
		}
		if r.Method == http.MethodPut {
			return cloudResponse(r, 202, `{"location":"/v1/api-jobs/job"}`), nil
		}
		return cloudResponse(r, 200, `{}`), nil
	})
	if err := c.PutAction(t.Context(), "/actions", map[string]any{}); err != nil || calls != 2 {
		t.Fatalf("server timeout call: calls=%d err=%v", calls, err)
	}
	var out any
	if err := c.Get(t.Context(), "/hosts", &out); err != nil {
		t.Fatal(err)
	}
}

func TestCloudMutationTimeoutIsUnknownAndIsNeverRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithRequestTimeout(time.Second), WithReadRetries(5))
		calls, start := 0, time.Now()
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		err := c.PutAction(t.Context(), "/actions", map[string]any{})
		apiErr := clientError(t, err, ErrorTimeout)
		if calls != 1 || time.Since(start) != time.Second || !apiErr.OutcomeUnknown || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timed-out write was replayed or treated as rejected: calls=%d unknown=%v err=%v", calls, apiErr.OutcomeUnknown, err)
		}
	})
}

func TestCloudActionCanceledBeforeSubmissionHasNoUnknownMutation(t *testing.T) {
	c := clientForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("canceled action was submitted")
		return nil, nil
	})
	err := c.PutAction(ctx, "/actions", map[string]any{})
	apiErr := clientError(t, err, ErrorCanceled)
	if apiErr.OutcomeUnknown || apiErr.JobID != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("unsubmitted action became unknown: %+v", apiErr)
	}
}

func TestCloudJobQueryRetryExhaustionRetainsJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := clientForTest(t, WithReadRetries(2), WithRetryBackoff(time.Second, time.Second))
		writes, polls := 0, 0
		c.http.Transport = cloudTransportFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPut {
				writes++
				return cloudResponse(r, 202, `{"location":"/v1/api-jobs/job"}`), nil
			}
			polls++
			return nil, io.EOF
		})
		err := c.PutAction(t.Context(), "/actions", map[string]any{})
		apiErr := clientError(t, err, ErrorNetwork)
		if writes != 1 || polls != 3 || apiErr.JobID != "job" || !apiErr.OutcomeUnknown || !errors.Is(err, io.EOF) {
			t.Fatalf("job query failure lost outstanding mutation: writes=%d polls=%d job=%s unknown=%v err=%v", writes, polls, apiErr.JobID, apiErr.OutcomeUnknown, err)
		}
	})
}
