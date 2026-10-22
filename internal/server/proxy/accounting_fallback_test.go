package proxy

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The gateway's TPOT averages the time from the first generated token to the
// last over the tokens after the first, so an engine that sends a round of
// tokens per event is measured per token; a reply without usage, with fewer
// than two tokens or with nothing streamed has no TPOT.
func TestGatewayTPOT(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stats responseStats
		want  float64
		ok    bool
	}{
		{"41 tokens over 2s", responseStats{usage: true, completion: 41, hasFirstToken: true, firstToken: 500 * time.Millisecond, lastEvent: 2500 * time.Millisecond}, 50, true},
		{"one event carrying the whole round", responseStats{usage: true, completion: 9, hasFirstToken: true, firstToken: 100 * time.Millisecond, lastEvent: 180 * time.Millisecond}, 10, true},
		{"no usage", responseStats{completion: 41, hasFirstToken: true, firstToken: time.Second, lastEvent: 2 * time.Second}, 0, false},
		{"a single token", responseStats{usage: true, completion: 1, hasFirstToken: true, firstToken: time.Second, lastEvent: time.Second}, 0, false},
		{"not streamed", responseStats{usage: true, completion: 41}, 0, false},
		{"everything in one read", responseStats{usage: true, completion: 41, hasFirstToken: true, firstToken: time.Second, lastEvent: time.Second}, 0, false},
	} {
		got, ok := gatewayTPOTMS(tc.stats)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: tpot = %v (%v), want %v (%v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// A deployment records exactly the fallbacks its route reading names, from
// the reply's own figures, and nothing for a fallback the reply gives no
// value for.
func TestResponseCountersRecordOnlyTheDeploymentsFallbacks(t *testing.T) {
	stats := responseStats{
		observed: true, usage: true, completion: 21, hasFirstToken: true,
		firstToken: 200 * time.Millisecond, lastEvent: 1200 * time.Millisecond,
		engine: map[string]float64{inferencemetrics.RequestPrefillSeconds: 0.111, inferencemetrics.RequestDecodeSeconds: 2.272},
	}
	labels := map[string]string{"deployment": "7"}
	for _, tc := range []struct {
		engine string
		want   []string
	}{
		// vLLM exports all four itself.
		{"vllm", nil},
		// SGLang exports TPOT and the queue time.
		{"sglang", []string{"gpuworks_proxy_reported_prefill_ms_count", "gpuworks_proxy_reported_decode_ms_count"}},
		// The reply carries no queue time, so there is none to record.
		{"callable", []string{"gpuworks_proxy_tpot_ms_count", "gpuworks_proxy_reported_prefill_ms_count", "gpuworks_proxy_reported_decode_ms_count"}},
	} {
		var r responseCounters
		r.record(time.Second, stats, inferencemetrics.GatewayFallbacksFor(tc.engine))
		samples := r.fallbackSamples(labels, 1)
		var counts []string
		for _, s := range samples {
			if strings.HasSuffix(s.Metric, "_count") {
				counts = append(counts, s.Metric)
			}
		}
		if strings.Join(counts, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: recorded %v, want %v", tc.engine, counts, tc.want)
		}
	}

	var r responseCounters
	r.record(time.Second, stats, inferencemetrics.GatewayFallbacksFor("callable"))
	samples := r.fallbackSamples(labels, 1)
	for metric, want := range map[string]float64{
		"gpuworks_proxy_tpot_ms_sum":             50,
		"gpuworks_proxy_reported_prefill_ms_sum": 111,
		"gpuworks_proxy_reported_decode_ms_sum":  2272,
	} {
		if got, ok := sampleValue(samples, metric, labels); !ok || math.Abs(got-want) > 1e-6 {
			t.Errorf("%s = %v (present %v), want %v", metric, got, ok, want)
		}
	}
	// 50ms lands in the 50 bucket, cumulative up to +Inf.
	if got, _ := sampleValue(samples, "gpuworks_proxy_tpot_ms_bucket", map[string]string{"le": "35"}); got != 0 {
		t.Errorf("tpot le=35 = %v, want 0", got)
	}
	if got, _ := sampleValue(samples, "gpuworks_proxy_tpot_ms_bucket", map[string]string{"le": "50"}); got != 1 {
		t.Errorf("tpot le=50 = %v, want 1", got)
	}
	if got, _ := sampleValue(samples, "gpuworks_proxy_tpot_ms_bucket", map[string]string{"le": "+Inf"}); got != 1 {
		t.Errorf("tpot le=+Inf = %v, want 1", got)
	}
}

// A real TensorFold reply forwarded through the gateway for a callable
// deployment whose template declares TensorFold's request stats reaches the
// client unchanged and fills the deployment's TPOT and phase histograms; the
// same reply on a vLLM deployment fills none of them.
func TestGatewayRecordsFallbacksForAnEngineWithoutThem(t *testing.T) {
	stream := readFixture(t, "tensorfold-gb10-stream.sse")
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := strings.SplitAfter(stream, "\n\n")
		for i, event := range events {
			_, _ = io.WriteString(w, event)
			w.(http.Flusher).Flush()
			if i > 0 && i < len(events)-2 {
				time.Sleep(2 * time.Millisecond)
			}
		}
	})
	contract, _ := json.Marshal(map[string]any{
		"operations":    []any{},
		"request_stats": map[string]string{"tensorfold.prefill_s": "prefill_seconds", "tensorfold.decode_s": "decode_seconds"},
	})
	snapshot, err := deploy.CatalogSelection{Engine: "callable", APIContract: contract}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		shape *store.Deployment
		want  bool
	}{
		{"callable", &store.Deployment{Engine: "callable", CatalogSelectionSnapshot: string(snapshot)}, true},
		{"vllm", &store.Deployment{Engine: "vllm", CatalogSelectionSnapshot: string(snapshot)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, acct, apiKey, depID, closeUpstream := newAccountingProxyFor(t, upstream, tc.shape)
			defer closeUpstream()
			req := httptest.NewRequest(http.MethodPost, chatPath, strings.NewReader(`{"model":"omni","stream":true}`))
			req.Header.Set("Authorization", "Bearer "+apiKey)
			rec := httptest.NewRecorder()
			p.Handler()(rec, req)
			if rec.Body.String() != stream {
				t.Fatalf("the client received %d bytes, the engine sent %d", rec.Body.Len(), len(stream))
			}
			_, samples := acct.snapshotAndClearDirty()
			dep := map[string]string{"deployment": itoa(depID)}
			for metric, want := range map[string]float64{
				"gpuworks_proxy_tpot_ms_count":             1,
				"gpuworks_proxy_reported_prefill_ms_sum":   111.2,
				"gpuworks_proxy_reported_decode_ms_sum":    2272,
				"gpuworks_proxy_reported_prefill_ms_count": 1,
			} {
				got, ok := sampleValue(samples, metric, dep)
				if !tc.want {
					if ok {
						t.Errorf("%s written for a vLLM deployment", metric)
					}
					continue
				}
				if !ok || math.Abs(got-want) > 1e-6 {
					t.Errorf("%s = %v (present %v), want %v", metric, got, ok, want)
				}
			}
			if _, ok := sampleValue(samples, "gpuworks_proxy_reported_queue_ms_count", dep); ok {
				t.Error("a queue time was recorded though TensorFold reports none")
			}
		})
	}
}
