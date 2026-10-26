package seed

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/inferencemetrics"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
)

// The request stats a callable template declares name numbers the engine
// really writes into a reply's last event. Captures from TensorFold v0.6.5
// and stock Strata 0.1.38 cover native prefill/decode. Strata's runtime patch
// additionally exports queue_ms, independently verified against its actual
// HTTP handler by build/strata-queue/integration; its value here is modeled,
// not claimed to be present in the historical stock capture.
func TestCallableTemplatesDeclareRequestStatsTheirRepliesCarry(t *testing.T) {
	for _, tc := range []struct {
		model, spec, reply string
		runtimePatched     bool
	}{
		{tensorfoldGLMModel, tensorfoldGB10Spec, "tensorfold-gb10-stream.sse", false},
		{tensorfoldGLMModel, tensorfoldTP4Spec, "tensorfold-gb10-stream.sse", false},
		{"ista-daslab/qwen3.8-flash-next-strata", "iq2xs-strata-nvidia-single-gpu", "strata-stream.sse", true},
	} {
		_, spec := ppuSpec(t, tc.model, tc.spec)
		if spec.API == nil || len(spec.API.RequestStats) == 0 {
			t.Fatalf("%s declares no request stats", tc.spec)
		}
		last := lastReplyEvent(t, "../../proxy/testdata/"+tc.reply)
		if tc.runtimePatched {
			stockTimings, ok := last["timings"].(map[string]any)
			if !ok {
				t.Fatal("stock Strata capture has no timings object")
			}
			if _, exists := stockTimings["queue_ms"]; exists {
				t.Fatal("historical stock Strata capture must preserve unknown queue timing")
			}
			// Model only the optional runtime extension; leave captured native
			// fields intact and never turn the stock capture's unknown into zero.
			patchedTimings := make(map[string]any, len(stockTimings)+1)
			for key, value := range stockTimings {
				patchedTimings[key] = value
			}
			patchedTimings["queue_ms"] = float64(20300)
			last["timings"] = patchedTimings
		}
		stats := map[string]bool{}
		for _, rule := range inferenceapi.ResolveRequestStats(spec.Engine, spec.API.RequestStats) {
			var current any = last
			for _, key := range rule.Keys() {
				object, ok := current.(map[string]any)
				if !ok {
					t.Fatalf("%s: %s is not inside an object", tc.spec, rule.Path)
				}
				current = object[key]
			}
			if value, ok := current.(float64); !ok || value*rule.Factor() <= 0 || value*rule.Factor() > 60 {
				t.Errorf("%s: %s is %v in the reply, not a duration in seconds", tc.spec, rule.Path, current)
			}
			if tc.runtimePatched && rule.Stat == inferencemetrics.RequestQueueSeconds {
				if value, ok := current.(float64); !ok || value*rule.Factor() != 20.3 {
					t.Errorf("%s: modeled 20300 ms queue maps to %v seconds, want 20.3", tc.spec, value*rule.Factor())
				}
			}
			stats[rule.Stat] = true
		}
		if tc.runtimePatched && !stats[inferencemetrics.RequestQueueSeconds] {
			t.Errorf("%s declares no runtime queue timing", tc.spec)
		}
		if !stats[inferencemetrics.RequestPrefillSeconds] || !stats[inferencemetrics.RequestDecodeSeconds] {
			t.Errorf("%s states %v, want prefill and decode", tc.spec, stats)
		}
	}
}

// lastReplyEvent is the last data event of a captured SSE reply.
func lastReplyEvent(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var last map[string]any
	for _, line := range strings.Split(string(raw), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		payload = strings.TrimSpace(payload)
		if !ok || payload == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(payload), &event); err == nil {
			last = event
		}
	}
	if last == nil {
		t.Fatalf("%s has no data event", path)
	}
	return last
}
