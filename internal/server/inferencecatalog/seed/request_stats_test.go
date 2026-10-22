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
// really writes into a reply's last event, read from the replies captured
// from TensorFold v0.6.5 and Strata 0.1.38; a renamed field fails here rather
// than as a request view with no phases. Prefill and decode are both stated,
// which is what splits a finished request's bar.
func TestCallableTemplatesDeclareRequestStatsTheirRepliesCarry(t *testing.T) {
	for _, tc := range []struct {
		model, spec, reply string
	}{
		{tensorfoldGLMModel, tensorfoldGB10Spec, "tensorfold-gb10-stream.sse"},
		{tensorfoldGLMModel, tensorfoldTP4Spec, "tensorfold-gb10-stream.sse"},
		{"ista-daslab/qwen3.8-flash-next-strata", "iq2xs-strata-nvidia-single-gpu", "strata-stream.sse"},
	} {
		_, spec := ppuSpec(t, tc.model, tc.spec)
		if spec.API == nil || len(spec.API.RequestStats) == 0 {
			t.Fatalf("%s declares no request stats", tc.spec)
		}
		last := lastReplyEvent(t, "../../proxy/testdata/"+tc.reply)
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
			stats[rule.Stat] = true
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
