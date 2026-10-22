package outbound

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type metadataRoundTrip func(*http.Request) (*http.Response, error)

func (f metadataRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMetadataRetriesTransientResponsesOnly(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 400, 401, 403, 404} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-15" {
					t.Errorf("range lost on retry")
				}
				if hits.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "archive header")
			}))
			defer server.Close()
			client := MetadataClient(server.Client())
			defer client.CloseIdleConnections()
			req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			req.Header.Set("Range", "bytes=0-15")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if status == 408 || status == 429 || status >= 500 {
				want = 2
				if resp.StatusCode != 206 || string(body) != "archive header" {
					t.Fatalf("retry result=%d %q", resp.StatusCode, body)
				}
			} else if resp.StatusCode != status {
				t.Fatalf("status=%d, want %d", resp.StatusCode, status)
			}
			if hits.Load() != want {
				t.Fatalf("requests=%d, want %d", hits.Load(), want)
			}
		})
	}
}

func TestMetadataAttemptDeadlineCanRecoverAndBodyRemainsReadable(t *testing.T) {
	var calls int
	transport := &metadataTransport{timeout: 5 * time.Millisecond, base: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("metadata")), Request: r}, nil
	})}
	client := &http.Client{Transport: transport}
	resp, err := client.Get("http://mirror.example/config.json")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Request.Context().Err() != nil {
		t.Fatal("body context canceled before the caller can read")
	}
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || string(b) != "metadata" || calls != 2 {
		t.Fatalf("body=%q err=%v calls=%d", b, err, calls)
	}
	if !errors.Is(resp.Request.Context().Err(), context.Canceled) {
		t.Fatal("closing body did not release attempt context")
	}
}

func TestMetadataCancellationStopsRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	client := MetadataClient(&http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		cancel()
		return nil, r.Context().Err()
	})})
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, "http://mirror.example/image.tar", nil)
	_, err := client.Do(req)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestMetadataRetryLimitAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter, method string
		want                     int
	}{
		{"bounded", "", http.MethodGet, 3},
		{"server requires longer wait", "60", http.MethodHead, 1},
		{"overflow wait", "9223372036854775807", http.MethodGet, 1},
		{"mutating request", "", http.MethodPost, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			client := MetadataClient(&http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{tc.retryAfter}}, Body: io.NopCloser(strings.NewReader("busy")), Request: r}, nil
			})})
			req, _ := http.NewRequest(tc.method, "http://mirror.example/meta", nil)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 503 || calls != tc.want {
				t.Fatalf("status=%d calls=%d, want %d", resp.StatusCode, calls, tc.want)
			}
		})
	}
}

func TestMetadataClientPreservesPolicyAndBoundsSlowBodies(t *testing.T) {
	sentinel := errors.New("redirect forbidden")
	base := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return sentinel }}
	client := MetadataClient(base)
	if base.Transport != nil || client.Timeout != base.Timeout || !errors.Is(client.CheckRedirect(nil, nil), sentinel) {
		t.Fatal("caller policy changed")
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	tr := &metadataTransport{base: server.Client().Transport, timeout: 20 * time.Millisecond}
	response, err := (&http.Client{Transport: tr}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("slow response body was not bounded")
	}
	if calls.Load() != 1 {
		t.Fatal("a body already delivered to a caller was retried")
	}
}

func TestMetadataDefaultAttemptAllowsMoreThanTenSeconds(t *testing.T) {
	client := MetadataClient(&http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) <= 10*time.Second || time.Until(deadline) > 30*time.Second {
			t.Fatalf("unexpected metadata deadline %v", deadline)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})})
	resp, err := client.Get("http://mirror.example/config.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestMetadataNetworkFailuresRetryOnlyWhenRecoverable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
		retries bool
	}{
		{"connection reset", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, true},
		{"connection refused", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, true},
		{"closed connection", io.EOF, true},
		{"permanent DNS", &net.DNSError{Err: "no such host", Name: "mirror.invalid", IsNotFound: true}, false},
		{"untrusted certificate", x509.UnknownAuthorityError{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			client := MetadataClient(&http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return nil, tc.failure
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			})})
			resp, err := client.Get("https://mirror.example/meta")
			if resp != nil {
				resp.Body.Close()
			}
			if tc.retries {
				if err != nil || calls != 2 {
					t.Fatalf("calls=%d error=%v", calls, err)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("permanent error retried: calls=%d error=%v", calls, err)
			}
		})
	}
}
