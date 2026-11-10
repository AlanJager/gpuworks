package benchrun

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

type deadlineTransport struct {
	mu        sync.Mutex
	remaining []time.Duration
}

func (d *deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if deadline, ok := r.Context().Deadline(); ok {
		d.mu.Lock()
		d.remaining = append(d.remaining, time.Until(deadline))
		d.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// go test ./internal/benchrun -run '^TestRunAppliesItsRequestTimeout$' -count=1
func TestRunAppliesItsRequestTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, search string
		want         time.Duration
	}{
		{"configured", `,"request_timeout_s":1800`, 30 * time.Minute},
		// A run stored before the setting existed takes today's default.
		{"unset", ``, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := (&fakeEndpoint{}).serve(t)
			transport := &deadlineTransport{}
			origin := &http.Client{Transport: transport, Timeout: 5 * time.Minute}
			rec := &recorder{}
			Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
				ID: 1, Model: "m", Mode: "grid", Client: loadgen.Client{HTTP: origin, BaseURL: srv.URL},
				Dataset: json.RawMessage(`{"kind":"random","ignore_eos":true}`),
				Search:  json.RawMessage(`{"concurrency":[1],"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"warmup_requests":1` + tc.search + `}`),
			}, rec.hooks())
			if done := rec.last(); done.State != "done" {
				t.Fatalf("terminal status = %+v", done)
			}
			// One warmup and one measured request, both on the run's timeout
			// and the origin's transport.
			if len(transport.remaining) != 2 {
				t.Fatalf("requests through the origin transport = %d, want 2", len(transport.remaining))
			}
			for _, remaining := range transport.remaining {
				if remaining > tc.want || remaining < tc.want-time.Minute {
					t.Fatalf("request deadline in %s, want about %s", remaining, tc.want)
				}
			}
			if origin.Timeout != 5*time.Minute {
				t.Fatalf("origin client timeout changed to %s", origin.Timeout)
			}
		})
	}
}

// go test ./internal/benchrun -run '^TestRequestSlowerThanTheTimeoutFails$' -count=1
func TestRequestSlowerThanTheTimeoutFails(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	rec := &recorder{}
	started := time.Now()
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 2, Model: "m", Mode: "grid", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random","ignore_eos":true}`),
		Search:  json.RawMessage(`{"concurrency":[1],"isl":[8],"osl":[3],"requests_per_point_multiplier":1,"request_timeout_s":1}`),
	}, rec.hooks())
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("run took %s; the one-second timeout did not bound the request", elapsed)
	}
	points := rec.points(t)
	if len(points) != 1 || points[0].Success != 0 || points[0].Errors != 1 {
		t.Fatalf("points = %+v", points)
	}
}
