package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const installLoginSecret = "fixture-private-password-and-response-key"

func installLoginRequest(t *testing.T, server *httptest.Server) clusterInstallNodeRequest {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return clusterInstallNodeRequest{Host: host, HTTP: n, Password: installLoginSecret}
}

func installLoginResponse(w http.ResponseWriter) {
	_ = json.NewEncoder(w).Encode(map[string]any{"token": "header.payload.signature", "user": map[string]any{"id": 1, "username": "admin", "role": "admin"}})
}

func TestClusterInstallWaitLoginReportsNormalizedLastFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		serve func(http.ResponseWriter)
		want  string
	}{
		{"unready", func(w http.ResponseWriter) { w.WriteHeader(503); _, _ = io.WriteString(w, installLoginSecret) }, "POST returned HTTP 503 (expected 200)"},
		{"unauthorized", func(w http.ResponseWriter) { w.WriteHeader(401); _, _ = io.WriteString(w, installLoginSecret) }, "POST returned HTTP 401 (expected 200)"},
		{"non-json", func(w http.ResponseWriter) { _, _ = io.WriteString(w, installLoginSecret) }, "installation API returned invalid JSON"},
		{"oversized", func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, installLoginSecret+strings.Repeat("x", 2<<20))
		}, "installation API response exceeds limit"},
		{"wrong-identity", func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"token": installLoginSecret, "user": map[string]any{"id": 1, "username": "admin", "role": "operator"}})
		}, "unexpected installation login identity"},
		{"partial-body", func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "4096")
			_, _ = io.WriteString(w, installLoginSecret)
		}, "installation login transport or response failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v1/auth/login" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				var body map[string]string
				if json.NewDecoder(r.Body).Decode(&body) != nil || body["username"] != "admin" || body["password"] != installLoginSecret {
					t.Error("installation login request differs")
				}
				tc.serve(w)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 450*time.Millisecond)
			defer cancel()
			token, err := clusterInstallWaitLogin(ctx, installLoginRequest(t, server))
			if token != "" || !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("last failure lost: token present=%t err=%v", token != "", err)
			}
			if strings.Contains(err.Error(), installLoginSecret) || strings.Contains(err.Error(), "header.payload.signature") {
				t.Fatal("private response or credentials leaked")
			}
			if calls.Load() < 1 || calls.Load() > 3 {
				t.Fatalf("original retry interval was not retained: %d calls", calls.Load())
			}
		})
	}
}

func TestClusterInstallWaitLoginDeadlineRequestKeepsEarlierHTTPFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 450*time.Millisecond)
	defer cancel()
	_, err := clusterInstallWaitLogin(ctx, installLoginRequest(t, server))
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "POST returned HTTP 503") {
		t.Fatalf("terminal context request overwrote prior HTTP failure: %v", err)
	}
	if calls.Load() != 2 || strings.Contains(err.Error(), "transport or response failure") {
		t.Fatalf("unexpected deadline diagnostic/calls: %v / %d", err, calls.Load())
	}
}

func TestClusterInstallWaitLoginDelayedSuccessKeepsRetryInterval(t *testing.T) {
	var mu sync.Mutex
	var starts []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		starts = append(starts, time.Now())
		n := len(starts)
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		installLoginResponse(w)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	token, err := clusterInstallWaitLogin(ctx, installLoginRequest(t, server))
	if err != nil || token != "header.payload.signature" {
		t.Fatalf("delayed login did not succeed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 3 {
		t.Fatalf("unexpected requests: %d", len(starts))
	}
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 200*time.Millisecond {
			t.Fatal("installation login retry interval shortened")
		}
	}
}

func TestClusterInstallWaitLoginCancellationAndRefusedTransport(t *testing.T) {
	t.Run("cancel-in-flight", func(t *testing.T) {
		started := make(chan struct{})
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			calls.Add(1)
			close(started)
			<-r.Context().Done()
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		req := installLoginRequest(t, server)
		go func() { _, err := clusterInstallWaitLogin(ctx, req); done <- err }()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("real login request did not start")
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), installLoginSecret) || calls.Load() != 1 {
				t.Fatalf("cancellation contract differs: %v / %d", err, calls.Load())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("installation login did not honor cancellation")
		}
	})
	t.Run("already-canceled", func(t *testing.T) {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := clusterInstallWaitLogin(ctx, installLoginRequest(t, server))
		if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
			t.Fatalf("canceled operation sent request: %v / %d", err, calls.Load())
		}
	})
	t.Run("connection-refused", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("closed listener accepted request") }))
		req := installLoginRequest(t, server)
		server.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := clusterInstallWaitLogin(ctx, req)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "installation login transport or response failure") {
			t.Fatalf("refused connection diagnostic lost: %v", err)
		}
		if strings.Contains(err.Error(), fmt.Sprint(req.HTTP)) || strings.Contains(err.Error(), installLoginSecret) {
			t.Fatal("raw transport or credentials leaked")
		}
	})
}
