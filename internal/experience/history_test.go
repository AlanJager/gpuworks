package experience

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	serverapp "gpuworks.ai/gpuworks/internal/server/app"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func series(points ...tsdb.Point) tsdb.Series {
	return tsdb.Series{Labels: map[string]string{"node": "n"}, Points: points}
}

func TestBackfillCounterMeetsLivePoint(t *testing.T) {
	now := time.Now()
	first := now.Add(-2 * time.Minute)
	s := series(tsdb.Point{TimeMS: first.UnixMilli(), Value: 1e9}, tsdb.Point{TimeMS: now.UnixMilli(), Value: 1e9 + 1200})
	out := backfillSeries("gpuworks_infer_prompt_tokens_total", s, metricShape{counter: true, follow: 0.05}, now, rand.New(rand.NewPCG(1, 2)))
	if len(out) < 1000 {
		t.Fatalf("%d samples for a day", len(out))
	}
	for i := 1; i < len(out); i++ {
		if out[i].Value < out[i-1].Value || out[i].TimeMS <= out[i-1].TimeMS {
			t.Fatalf("not growing at %d: %v then %v", i, out[i-1], out[i])
		}
	}
	last := out[len(out)-1]
	if last.TimeMS >= first.UnixMilli() || last.Value > 1e9 {
		t.Fatalf("history ends after or above the live point: %+v", last)
	}
	// It meets the live point within one step of growth (10 tokens a second).
	if 1e9-last.Value > 10*backfillStep.Seconds()*1.2 {
		t.Fatalf("gap to the live point: %v", 1e9-last.Value)
	}
	if first.Sub(time.UnixMilli(out[0].TimeMS)) < backfillSpan-2*backfillStep {
		t.Fatalf("history starts at %v", time.UnixMilli(out[0].TimeMS))
	}
}

func TestBackfillYoungCounterStartsAtZero(t *testing.T) {
	now := time.Now()
	s := series(tsdb.Point{TimeMS: now.Add(-time.Minute).UnixMilli(), Value: 5}, tsdb.Point{TimeMS: now.UnixMilli(), Value: 65})
	out := backfillSeries("gpuworks_proxy_requests_total", s, metricShape{counter: true, follow: 0.05}, now, rand.New(rand.NewPCG(1, 2)))
	if len(out) == 0 || out[0].Value <= 0 || out[0].Value > 1*backfillStep.Seconds()*1.2*4 {
		t.Fatalf("first value %v", out[0].Value)
	}
	if out[len(out)-1].Value <= 5 {
		t.Fatalf("a counter younger than the day should read as restarted at the join, ends at %v", out[len(out)-1].Value)
	}
}

func TestBackfillShortSpan(t *testing.T) {
	now := time.Now()
	s := series(tsdb.Point{TimeMS: now.Add(-10 * time.Second).UnixMilli(), Value: 1}, tsdb.Point{TimeMS: now.UnixMilli(), Value: 2})
	if out := backfillSeries("x_total", s, metricShape{counter: true}, now, rand.New(rand.NewPCG(1, 2))); out != nil {
		t.Fatalf("a counter seen for 10 s has no rate, got %d samples", len(out))
	}
	if out := backfillSeries("x", tsdb.Series{}, metricShape{}, now, rand.New(rand.NewPCG(1, 2))); out != nil {
		t.Fatal("an empty series got history")
	}
}

func TestBackfillGauges(t *testing.T) {
	now := time.Now()
	s := series(tsdb.Point{TimeMS: now.Add(-time.Minute).UnixMilli(), Value: 40}, tsdb.Point{TimeMS: now.UnixMilli(), Value: 60})
	out := backfillSeries("gpuworks_gpu_utilization", s, backfilled["gpuworks_gpu_utilization"], now, rand.New(rand.NewPCG(1, 2)))
	sum := 0.0
	for _, p := range out {
		if p.Value < 0 || p.Labels["node"] != "n" || p.Metric != "gpuworks_gpu_utilization" {
			t.Fatalf("sample %+v", p)
		}
		sum += p.Value
	}
	if mean := sum / float64(len(out)); mean < 10 || mean > 150 {
		t.Fatalf("mean %v far from the live level 50", mean)
	}
	steady := backfillSeries("gpuworks_gpu_memory_total_bytes", series(tsdb.Point{TimeMS: now.UnixMilli(), Value: 8}), metricShape{steady: true}, now, rand.New(rand.NewPCG(1, 2)))
	for _, p := range steady {
		if p.Value != 8 {
			t.Fatalf("steady gauge moved: %v", p.Value)
		}
	}
	rounded := backfillSeries("gpuworks_infer_requests_running", s, backfilled["gpuworks_infer_requests_running"], now, rand.New(rand.NewPCG(1, 2)))
	for _, p := range rounded {
		if p.Value != math.Round(p.Value) {
			t.Fatalf("count gauge %v", p.Value)
		}
	}
}

func TestBackfilledCounterFamilies(t *testing.T) {
	for _, name := range []string{"gpuworks_infer_ttft_seconds_sum", "gpuworks_infer_ttft_seconds_count", "gpuworks_proxy_routed_total"} {
		if !backfilled[name].counter {
			t.Errorf("%s is not backfilled as a counter", name)
		}
	}
	if backfilled["gpuworks_infer_ttft_seconds"].counter {
		t.Error("the quantile gauge is backfilled as a counter")
	}
}

func TestCardHistory(t *testing.T) {
	end := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	start := end.AddDate(0, 0, -30)
	members := map[string]member{"gpu-a100-01": {UUID: "a100"}}
	key := func(i int) rollup.GPUKey {
		return rollup.GPUKey{NodeUUID: "a100", GPUIndex: i, GPUUUID: "u" + string(rune('0'+i))}
	}
	const mem = 80 << 30
	cards := []serverapp.StatisticsCard{
		{Key: key(0), Model: "A100", MemoryTotal: mem, Holders: map[uint]float64{1: 0.3}}, // dGPU slice of qwen3-8b-chat
		{Key: key(1), Model: "A100", MemoryTotal: mem, Holders: map[uint]float64{4: 1}},   // qwen3-32b-rag, 16 days
		{Key: key(2), Model: "A100", MemoryTotal: mem, Holders: map[uint]float64{}},
	}
	ids := map[string]uint{"qwen3-8b-chat": 1, "qwen3-32b-rag": 4}
	h := cardHistory(start, end, cards, ids, members)

	hours := int(end.Sub(start).Hours())
	if len(h.GPUs) != 3*hours {
		t.Fatalf("%d card hours, want %d", len(h.GPUs), 3*hours)
	}
	for k, c := range h.Capacity {
		if c.TotalGPUSeconds != 3*3600 || c.SchedulableGPUSeconds != 3*3600 || k.GPUModel != "A100" {
			t.Fatalf("capacity %+v %+v", k, c)
		}
	}
	noon := end.Add(-24 * time.Hour).UnixMilli()
	util := func(i int, ms int64) float64 {
		k := key(i)
		k.HourMS = ms
		g := h.GPUs[k]
		return g.UtilSum / float64(g.UtilN)
	}
	if !(util(1, noon) > util(2, noon)+20) {
		t.Fatalf("held card %v, idle card %v", util(1, noon), util(2, noon))
	}
	early := start.UnixMilli()
	if util(1, early) > 5 {
		t.Fatalf("the RAG card is busy before the deployment went live: %v", util(1, early))
	}
	if _, ok := h.GPUTime[rollup.GPUTimeKey{HourMS: early, DeploymentID: 4, GPUModel: "A100"}]; ok {
		t.Fatal("card time before the deployment went live")
	}
	slice := h.GPUTime[rollup.GPUTimeKey{HourMS: noon, DeploymentID: 1, GPUModel: "A100"}]
	if math.Abs(slice.GPUSeconds-0.3*3600) > 1e-6 || slice.WholeGPUSeconds != 3600 || slice.MaxInstances != 1 {
		t.Fatalf("dGPU card time %+v", slice)
	}
	rag := h.GPUTime[rollup.GPUTimeKey{HourMS: noon, DeploymentID: 4, GPUModel: "A100"}]
	if rag.GPUSeconds != 3600 || rag.MaxInstances != 2 {
		t.Fatalf("rag card time %+v", rag)
	}
	named := key(0)
	if h.GPUModels[named] != "A100" {
		t.Fatalf("models %v", h.GPUModels)
	}
}

func TestRequestHistory(t *testing.T) {
	end := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	start := end.AddDate(0, 0, -30)
	ids := map[string]uint{}
	for i, st := range starters {
		ids[st.Name] = uint(i + 1)
	}
	keys := map[string][2]uint{}
	for i, cl := range callers {
		keys[cl.Name] = [2]uint{uint(i + 1), 1}
	}
	recs, weights := requestHistory(start, end, ids, keys)
	if len(recs) == 0 || len(recs) != len(weights) {
		t.Fatalf("%d records, %d weights", len(recs), len(weights))
	}
	ascend := ids["qwen3-32b-ascend"]
	liveFrom := end.AddDate(0, 0, -9).UnixMilli()
	failed := 0
	for i, r := range recs {
		if weights[i] <= 0 {
			t.Fatalf("weight %d", weights[i])
		}
		if r.DeploymentID == ascend && r.ReceivedMS < liveFrom {
			t.Fatalf("a request before the deployment went live: %v", time.UnixMilli(r.ReceivedMS))
		}
		if r.ReceivedMS < start.UnixMilli() || r.ReceivedMS >= end.Add(time.Hour).UnixMilli() {
			t.Fatalf("request outside the range: %v", time.UnixMilli(r.ReceivedMS))
		}
		if r.Outcome == uxrecord.OutcomeUpstreamError {
			failed++
			if r.Status != 503 || r.Usage {
				t.Fatalf("failed record %+v", r)
			}
		} else if r.CachedTokens > r.PromptTokens {
			t.Fatalf("more cached than prompt tokens: %+v", r)
		}
	}
	if failed == 0 {
		t.Fatal("no failed requests in a month")
	}
	if recs2, _ := requestHistory(start, end, map[string]uint{}, keys); len(recs2) != 0 {
		t.Fatal("history for deployments that do not exist")
	}
}
