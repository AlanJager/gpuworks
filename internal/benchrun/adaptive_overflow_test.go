package benchrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

func TestSLARunWithHugeConcurrencyAndSmallSessions(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	start := maxInt/2 + 1
	for _, limit := range []int{start + 1, 0} {
		t.Run(fmt.Sprintf("max=%d", limit), func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer srv.Close()
			end := limit
			if end == 0 {
				end = maxInt
			}
			raw := fmt.Sprintf(`{"concurrency":%d,"max_concurrency":%d,"isl":[1],"osl":[1],"sessions_per_point":{"%d":1,"%d":1}}`, start, limit, start, end)
			rec := &recorder{}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			Execute(ctx, Run{TokenizerRef: testTokenizerRef(t), ID: 1, Model: "m", Mode: "sla", NoStream: true, Client: loadgen.Client{BaseURL: srv.URL}, Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(raw)}, rec.hooks())
			if last := rec.last(); last.State != "done" || last.StopReason != proto.BenchmarkStopHitMaxConcurrency {
				t.Fatalf("terminal=%+v", last)
			}
			points := rec.points(t)
			if len(points) != 2 || points[0].Concurrency != start || points[1].Concurrency != end || points[0].Success != 1 || points[1].Success != 1 || calls.Load() != 2 {
				t.Fatalf("points=%+v calls=%d", points, calls.Load())
			}
		})
	}
}
