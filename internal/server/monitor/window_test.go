package monitor

import (
	"fmt"
	"testing"
	"time"
)

// baseTime is a fixed reference so tests don't depend on wall-clock time.
var baseTime = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

func TestComputeWindowNoDataLeavesHasFlagsFalse(t *testing.T) {
	store := newTestStore(t)
	w := computeWindowFromStore(t, store, 1, nil, baseTime, 5*time.Minute)
	if w.HasRunning || w.HasWaiting || w.HasKVUsage || w.HasKVHitRate || w.HasTokens {
		t.Fatalf("expected every Has* flag false for an empty store, got %+v", w)
	}
}

func TestComputeWindowRunningWaitingAndKV(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 7}}

	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ { // 8 ticks * 10s
		at := start.Add(time.Duration(i) * 10 * time.Second)
		labels := map[string]string{"instance": "inst-a", "deployment": "7"}
		writeSample(t, store, "gpuworks_infer_requests_running", labels, 3, at)
		writeSample(t, store, "gpuworks_infer_requests_waiting", labels, 2, at)
		writeSample(t, store, "gpuworks_infer_gpu_cache_usage_percent", labels, 0.95, at)
	}

	w := computeWindowFromStore(t, store, 7, instances, baseTime, 5*time.Minute)
	if !w.HasRunning || w.RunningAvg != 3 {
		t.Errorf("running: has=%v avg=%v, want has=true avg=3", w.HasRunning, w.RunningAvg)
	}
	if !w.HasWaiting || w.WaitingP95 != 2 {
		t.Errorf("waiting: has=%v p95=%v, want has=true p95=2", w.HasWaiting, w.WaitingP95)
	}
	if !w.HasKVUsage || w.KVUsageMax != 0.95 {
		t.Errorf("kv: has=%v max=%v, want has=true max=0.95", w.HasKVUsage, w.KVUsageMax)
	}
}

func TestComputeWindowFallsBackToInstanceMatcherForUnlabeledSamples(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-old", DeploymentID: 9}}

	// An old agent that predates the "deployment" label never attaches it.
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "inst-old"}, 4, baseTime)

	w := computeWindowFromStore(t, store, 9, instances, baseTime, 5*time.Minute)
	if !w.HasRunning || w.RunningAvg != 4 {
		t.Fatalf("expected the fallback instance-matcher query to find the sample, got %+v", w)
	}
}

func TestComputeWindowKVHitRatePrefersVLLMCountersOverSGLangGauge(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 1}}
	labels := map[string]string{"instance": "inst-a", "deployment": "1"}

	writeSample(t, store, "gpuworks_infer_kv_queries_total", labels, 100, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_kv_queries_total", labels, 200, baseTime)
	writeSample(t, store, "gpuworks_infer_kv_hits_total", labels, 50, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_kv_hits_total", labels, 130, baseTime)
	// An instant gauge is also present but must be ignored: counters win.
	writeSample(t, store, "gpuworks_infer_kv_hit_rate_instant", labels, 0.1, baseTime)

	w := computeWindowFromStore(t, store, 1, instances, baseTime, 5*time.Minute)
	if !w.HasKVHitRate {
		t.Fatal("expected HasKVHitRate")
	}
	// (130-50)/(200-100) = 0.8
	if got, want := w.KVHitRate, 0.8; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("KVHitRate = %v, want %v", got, want)
	}
}

func TestComputeWindowKVHitRateFallsBackToInstantGauge(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 2}}
	labels := map[string]string{"instance": "inst-a", "deployment": "2"}

	writeSample(t, store, "gpuworks_infer_kv_hit_rate_instant", labels, 0.42, baseTime)

	w := computeWindowFromStore(t, store, 2, instances, baseTime, 5*time.Minute)
	if !w.HasKVHitRate || w.KVHitRate != 0.42 {
		t.Fatalf("KVHitRate = %v has=%v, want 0.42/true", w.KVHitRate, w.HasKVHitRate)
	}
}

func TestComputeWindowTokenRates(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 3}}
	labels := map[string]string{"instance": "inst-a", "deployment": "3"}

	// A real scrape tick reports requests_running alongside the token
	// counters (same /metrics call, same labels) - included here so
	// coverage (derived from requests_running) correctly finds inst-a
	// already deployment-labeled instead of treating it as uncovered.
	writeSample(t, store, "gpuworks_infer_requests_running", labels, 1, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_requests_running", labels, 1, baseTime)
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", labels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", labels, 100, baseTime)
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 50, baseTime)

	w := computeWindowFromStore(t, store, 3, instances, baseTime, 5*time.Minute)
	if !w.HasTokens {
		t.Fatal("expected HasTokens")
	}
	if w.PromptTokensPerSec != 10 {
		t.Errorf("PromptTokensPerSec = %v, want 10", w.PromptTokensPerSec)
	}
	if w.GenTokensPerSec != 5 {
		t.Errorf("GenTokensPerSec = %v, want 5", w.GenTokensPerSec)
	}
}

// TestComputeWindowTokenRatesWithoutRequestsRunning is the hardening
// regression case: a Callable/custom engine spec may map prompt/generation
// token counters without ever emitting gpuworks_infer_requests_running at
// all. Coverage-from-running then sees no data and treats inst-a as
// "uncovered", but queryDeploymentMetric's own per-metric check must still
// find inst-a already present in the token metrics' own deployment-matched
// result and skip the fallback query, so the rate is not double counted.
func TestComputeWindowTokenRatesWithoutRequestsRunning(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 33}}
	labels := map[string]string{"instance": "inst-a", "deployment": "33"}

	// Deliberately no gpuworks_infer_requests_running sample at all.
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", labels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", labels, 100, baseTime)
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 50, baseTime)

	w := computeWindowFromStore(t, store, 33, instances, baseTime, 5*time.Minute)
	if w.HasRunning {
		t.Error("expected HasRunning false: no requests_running sample was written")
	}
	if !w.HasTokens {
		t.Fatal("expected HasTokens")
	}
	if w.PromptTokensPerSec != 10 {
		t.Errorf("PromptTokensPerSec = %v, want 10 (not doubled)", w.PromptTokensPerSec)
	}
	if w.GenTokensPerSec != 5 {
		t.Errorf("GenTokensPerSec = %v, want 5 (not doubled)", w.GenTokensPerSec)
	}
}

// TestComputeWindowFallbackCoversOnlyUncoveredInstances is F1's regression
// case: a deployment-matched query that already covers some replicas (new
// agent, labeled) must still pick up the ones it doesn't cover (old agent,
// unlabeled) instead of treating any non-empty result as "fully covered".
func TestComputeWindowFallbackCoversOnlyUncoveredInstances(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{
		{UUID: "labeled", DeploymentID: 20},
		{UUID: "unlabeled", DeploymentID: 20},
	}
	// "labeled" is a new agent: both instance and deployment labels.
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "labeled", "deployment": "20"}, 4, baseTime)
	// "unlabeled" is a pre-upgrade agent: instance label only.
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "unlabeled"}, 4, baseTime)

	w := computeWindowFromStore(t, store, 20, instances, baseTime, 5*time.Minute)
	if !w.HasRunning || w.RunningAvg != 8 {
		t.Fatalf("RunningAvg = %v has=%v, want 8/true (both replicas' running=4 summed)", w.RunningAvg, w.HasRunning)
	}
}

// TestComputeWindowFallbackDoesNotDoubleCountAllUnlabeled is F1's other
// regression case: when every replica is unlabeled (the deployment-matched
// query returns nothing at all), falling back to per-instance queries must
// not somehow count any replica twice.
func TestComputeWindowFallbackDoesNotDoubleCountAllUnlabeled(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{
		{UUID: "a", DeploymentID: 21},
		{UUID: "b", DeploymentID: 21},
	}
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "a"}, 4, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "b"}, 4, baseTime)

	w := computeWindowFromStore(t, store, 21, instances, baseTime, 5*time.Minute)
	if !w.HasRunning || w.RunningAvg != 8 {
		t.Fatalf("RunningAvg = %v has=%v, want 8/true (4+4, not doubled)", w.RunningAvg, w.HasRunning)
	}
}

// TestComputeWindowFallbackDoesNotRequeryCoveredInstances is F1's
// double-count guard from the other direction: when every replica is
// already covered by the deployment-matched query, the fallback loop must
// not re-query (and so re-add) any of them.
func TestComputeWindowFallbackDoesNotRequeryCoveredInstances(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{
		{UUID: "a", DeploymentID: 22},
		{UUID: "b", DeploymentID: 22},
	}
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "a", "deployment": "22"}, 4, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "b", "deployment": "22"}, 4, baseTime)

	w := computeWindowFromStore(t, store, 22, instances, baseTime, 5*time.Minute)
	if !w.HasRunning || w.RunningAvg != 8 {
		t.Fatalf("RunningAvg = %v has=%v, want 8/true (4+4, not doubled by re-querying covered instances)", w.RunningAvg, w.HasRunning)
	}
}

// TestComputeWindowKVHitRateClampedAndCorrectAcrossMidWindowReset is F3's
// regression case: queries 1000,20,60,1100 / hits 900,18,55,1050 has a
// hidden reset between the first two points of each counter. Naive
// first-vs-last math gives (1050-900)/(1100-1000) = 1.5, an impossible
// ratio; walking every step with reset handling gives the true ratio, which
// must also come out clamped to at most 1.
func TestComputeWindowKVHitRateClampedAndCorrectAcrossMidWindowReset(t *testing.T) {
	store := newTestStore(t)
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 23}}
	labels := map[string]string{"instance": "inst-a", "deployment": "23"}

	queries := []float64{1000, 20, 60, 1100}
	hits := []float64{900, 18, 55, 1050}
	for i := range queries {
		at := baseTime.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_infer_kv_queries_total", labels, queries[i], at)
		writeSample(t, store, "gpuworks_infer_kv_hits_total", labels, hits[i], at)
	}

	w := computeWindowFromStore(t, store, 23, instances, baseTime.Add(30*time.Second), 5*time.Minute)
	if !w.HasKVHitRate {
		t.Fatal("expected HasKVHitRate")
	}
	if w.KVHitRate > 1 {
		t.Fatalf("KVHitRate = %v, must be clamped to at most 1", w.KVHitRate)
	}
	// query deltas: 20 (reset) + 40 + 1040 = 1100; hit deltas: 18 (reset) + 37
	// + 995 = 1050; ratio = 1050/1100.
	want := 1050.0 / 1100.0
	if got := w.KVHitRate; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("KVHitRate = %v, want %v", got, want)
	}
}

// TestFetchWindowSeriesPropagatesTSDBError is F12's coverage at the fetch
// level (the I/O now lives in FetchWindowSeries, not ComputeWindow itself):
// an error from any one metric query must surface, not be swallowed into a
// zeroed WindowSeries.
func TestFetchWindowSeriesPropagatesTSDBError(t *testing.T) {
	store := newTestStore(t)
	failing := &erroringStore{Store: store, failMetric: "gpuworks_infer_requests_waiting", err: fmt.Errorf("boom")}
	instances := []InstanceRef{{UUID: "inst-a", DeploymentID: 26}}
	_, err := FetchWindowSeries(failing, 26, instances, baseTime, baseTime.Add(-5*time.Minute), WindowStep.Milliseconds())
	if err == nil {
		t.Fatal("expected FetchWindowSeries to propagate the TSDB error")
	}
}

// TestComputeWindowFullyLabeledDeploymentIssuesNoFallbackQueries is N2's
// regression case: coverage derived once from gpuworks_infer_requests_running
// must be reused for every other metric, so a fully-labeled deployment never
// issues a per-instance fallback query - not even for a metric the engine
// never emits at all (kv_hit_rate_instant, SGLang-only, queried here for a
// vLLM-shaped deployment).
func TestComputeWindowFullyLabeledDeploymentIssuesNoFallbackQueries(t *testing.T) {
	store := newTestStore(t)
	recorder := &queryRecordingStore{Store: store}
	instances := []InstanceRef{
		{UUID: "r1", DeploymentID: 32},
		{UUID: "r2", DeploymentID: 32},
	}
	for _, uuid := range []string{"r1", "r2"} {
		labels := map[string]string{"instance": uuid, "deployment": "32"}
		writeSample(t, store, "gpuworks_infer_requests_running", labels, 1, baseTime)
		writeSample(t, store, "gpuworks_infer_requests_waiting", labels, 0, baseTime)
		writeSample(t, store, "gpuworks_infer_gpu_cache_usage_percent", labels, 0.1, baseTime)
		writeSample(t, store, "gpuworks_infer_kv_queries_total", labels, 10, baseTime)
		writeSample(t, store, "gpuworks_infer_kv_hits_total", labels, 5, baseTime)
		writeSample(t, store, "gpuworks_infer_prompt_tokens_total", labels, 10, baseTime)
		writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 5, baseTime)
		// No gpuworks_infer_kv_hit_rate_instant: this deployment is vLLM-shaped.
	}

	_ = computeWindowFromStore(t, recorder, 32, instances, baseTime, 5*time.Minute)

	for _, c := range recorder.calls {
		if _, ok := c.matchers["instance"]; ok {
			t.Fatalf("unexpected per-instance fallback query for metric %s on a fully-labeled deployment: %+v", c.metric, c)
		}
	}
}
