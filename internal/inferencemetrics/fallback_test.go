package inferencemetrics

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/engines"
)

func gatewayNames(fallbacks []GatewayFallback) []string {
	var out []string
	for _, f := range fallbacks {
		out = append(out, f.Gateway)
	}
	return out
}

// vLLM exports TPOT and all three phase histograms itself, SGLang TPOT and
// the queue time; the gateway fills in only what each lacks, and everything
// for an engine whose exposition the platform does not know.
func TestGatewayFallbacksFollowTheEngineMapping(t *testing.T) {
	for _, tc := range []struct {
		engine string
		want   []string
	}{
		{engines.VLLM, nil},
		{engines.SGLang, []string{"reported_prefill_ms", "reported_decode_ms"}},
		{engines.Callable, []string{"tpot_ms", "reported_queue_ms", "reported_prefill_ms", "reported_decode_ms"}},
	} {
		if got := gatewayNames(GatewayFallbacksFor(tc.engine)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: fallbacks %v, want %v", tc.engine, got, tc.want)
		}
	}
}

// Every fallback names a gateway histogram, an engine histogram and, where it
// has one, a request stat of the vocabularies.
func TestGatewayFallbacksNameKnownMetrics(t *testing.T) {
	engineKinds := map[string]Kind{}
	for _, d := range Definitions() {
		engineKinds[d.Name] = d.Kind
	}
	for _, f := range GatewayFallbacks() {
		if Gateway(f.Gateway).Kind != Histogram {
			t.Errorf("%s is not a gateway histogram", f.Gateway)
		}
		if engineKinds[f.Engine] != Histogram {
			t.Errorf("%s stands in for %s, which is not an engine histogram", f.Gateway, f.Engine)
		}
		if f.Stat != "" && !IsRequestStat(f.Stat) {
			t.Errorf("%s reads unknown request stat %s", f.Gateway, f.Stat)
		}
	}
}
