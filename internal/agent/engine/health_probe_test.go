package engine

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeEngineHealthRetriesHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []int
		wantCalls int
		wantError bool
	}{
		{"healthy", []int{200}, 1, false},
		{"recovers", []int{503, 500, 200}, 3, false},
		{"unhealthy", []int{503}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/custom-health" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				index := min(int(calls.Add(1))-1, len(tc.statuses)-1)
				w.WriteHeader(tc.statuses[index])
				_, _ = w.Write([]byte("ok"))
			}))
			defer server.Close()
			spec := runningSpec("probe", serverPort(t, server))
			spec.HealthPath = "/custom-health"
			resp, err := probeEngineHealth(context.Background(), spec, server.Client(), 3, time.Millisecond)
			if (err != nil) != tc.wantError || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
			if resp != nil {
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || string(body) != "ok" {
					t.Fatalf("successful response was consumed: %q %v", body, err)
				}
			}
		})
	}
}

func TestProbeEngineHealthRetriesRequestTimeouts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	spec := runningSpec("timeout", serverPort(t, server))
	_, err := probeEngineHealth(context.Background(), spec, &http.Client{Timeout: 50 * time.Millisecond}, 3, time.Millisecond)
	if err == nil || calls.Load() != 3 || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("calls=%d error=%v", calls.Load(), err)
	}
}

type healthProbeTransport func(*http.Request) (*http.Response, error)

func (f healthProbeTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type healthProbeBody struct {
	io.Reader
	closed func()
}

func (b healthProbeBody) Close() error { b.closed(); return nil }

func TestProbeEngineHealthClosesEachFailedResponse(t *testing.T) {
	var closed int
	client := &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: healthProbeBody{Reader: strings.NewReader("unavailable"), closed: func() { closed++ }}}, nil
	})}
	_, err := probeEngineHealth(context.Background(), runningSpec("close", 1), client, 3, 0)
	if err == nil || closed != 3 {
		t.Fatalf("closed=%d error=%v", closed, err)
	}
}

func TestProbeEngineHealthCancellationIsNotAnEngineFailure(t *testing.T) {
	for _, duringRequest := range []bool{true, false} {
		name := "retry wait"
		if duringRequest {
			name = "request"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			var calls atomic.Int32
			client := &http.Client{Transport: healthProbeTransport(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if duringRequest {
					close(entered)
					<-req.Context().Done()
					return nil, req.Context().Err()
				}
				return &http.Response{StatusCode: 503, Header: make(http.Header), Body: healthProbeBody{Reader: strings.NewReader("unavailable"), closed: func() { close(entered) }}}, nil
			})}
			done := make(chan error, 1)
			go func() {
				_, err := probeEngineHealth(ctx, runningSpec("cancel", 1), client, 3, time.Minute)
				done <- err
			}()
			<-entered
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
					t.Fatalf("calls=%d error=%v", calls.Load(), err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt the probe")
			}
		})
	}
}
