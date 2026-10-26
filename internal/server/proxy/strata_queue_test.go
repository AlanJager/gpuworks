package proxy

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/inferencecatalog/seed"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestStrataCatalogQueueTimingReachesRequestRecord(t *testing.T) {
	catalog, err := seed.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	var rules inferenceapi.MetricMap
	for _, model := range catalog.Models {
		if model.ModelID == "ista-daslab/qwen3.8-flash-next-strata" {
			for _, spec := range model.Specs {
				if spec.Name == "iq2xs-strata-nvidia-single-gpu" && spec.API != nil {
					rules = spec.API.RequestStats
				}
			}
		}
	}
	if rule, ok := rules["timings.queue_ms"]; !ok || rule.To != "queue_seconds" || rule.Scale != 0.001 {
		t.Fatalf("Strata catalog has no millisecond queue observation: %+v", rules)
	}
	for _, tc := range []struct {
		name, timings string
		want          int32
	}{
		{"measured", `{"queue_ms":20300}`, 20300},
		{"zero", `{"queue_ms":0}`, 0},
		{"null", `{"queue_ms":null}`, uxrecord.Unknown},
		{"missing", `{}`, uxrecord.Unknown},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				// Literal response fields from the patched Strata handler. Fix the
				// clock value to catch unit conversion without a 20-second test.
				body := `{"id":"chatcmpl-queue-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1,"total_tokens":9},"timings":` + tc.timings + `}`
				header := jsonHeader()
				if stream {
					header = sseHeader()
					body = "data: {\"id\":\"chatcmpl-queue-test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-queue-test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1,\"total_tokens\":9},\"timings\":" + tc.timings + "}\n\ndata: [DONE]\n\n"
				}
				observer := newResponseObserver(chatPath, http.StatusOK, header, time.Unix(0, 0))
				observer.reading.rules = inferenceapi.ResolveRequestStats("callable", rules)
				feed(t, observer, body, 9)
				record := buildRecord(requestFacts{}, "instance", http.StatusOK, 21*time.Second, observer.stats(true), uxrecord.OutcomeOK, time.Time{})
				if record.EngineQueueMS != tc.want || record.EnginePrefillMS != uxrecord.Unknown {
					t.Fatalf("queue=%d prefill=%d, want queue=%d with unknown prefill", record.EngineQueueMS, record.EnginePrefillMS, tc.want)
				}
			})
		}
	}
}
