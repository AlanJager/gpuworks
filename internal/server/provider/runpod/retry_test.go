package runpod

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// dropFirst answers like RunPod behind an egress proxy that cuts the
// connection: the first n requests are closed without a response.
func dropFirst(t *testing.T, n int32, calls *atomic.Int32, ok http.HandlerFunc) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= n {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		ok(w, r)
	}
}

func TestIdempotentRequestsSurviveADroppedConnection(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			var calls atomic.Int32
			d := newTestDriver(t, dropFirst(t, 2, &calls, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != method || r.Header.Get("Authorization") != "Bearer key" {
					t.Errorf("request = %s %q", r.Method, r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusOK)
			}))
			d.repeatAfter = time.Millisecond
			resp, err := d.do(t.Context(), provider.Credentials{APIKey: "key"}, method, "/pods")
			if err != nil {
				t.Fatalf("%s after two dropped connections: %v", method, err)
			}
			resp.Body.Close()
			if calls.Load() != 3 {
				t.Fatalf("requests = %d, want 3", calls.Load())
			}
		})
	}
}

func TestRequestFailsAfterTheLastAttempt(t *testing.T) {
	var calls atomic.Int32
	d := newTestDriver(t, dropFirst(t, 99, &calls, nil))
	d.repeatAfter = time.Millisecond
	if _, err := d.do(t.Context(), provider.Credentials{APIKey: "key"}, http.MethodGet, "/pods"); err == nil {
		t.Fatal("a connection that never answers returned a response")
	}
	if calls.Load() != requestAttempts {
		t.Fatalf("requests = %d, want %d", calls.Load(), requestAttempts)
	}
}

// A create that is repeated might start a second billed pod.
func TestCreateIsNeverRepeated(t *testing.T) {
	var calls atomic.Int32
	d := newTestDriver(t, dropFirst(t, 99, &calls, nil))
	d.repeatAfter = time.Millisecond
	if _, err := d.doJSON(t.Context(), provider.Credentials{APIKey: "key"}, http.MethodPost, "/pods", map[string]any{"name": "p"}); err == nil {
		t.Fatal("a dropped create returned a response")
	}
	if calls.Load() != 1 {
		t.Fatalf("create was sent %d times, want 1", calls.Load())
	}
}

func TestRepeatingStopsWhenTheContextEnds(t *testing.T) {
	var calls atomic.Int32
	d := newTestDriver(t, dropFirst(t, 99, &calls, nil))
	d.repeatAfter = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := d.do(ctx, provider.Credentials{APIKey: "key"}, http.MethodGet, "/pods"); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("waited %s for a repeat after the context ended", time.Since(started))
	}
}
