package rollup

import (
	"math"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

func TestDaysInTheStatisticsZone(t *testing.T) {
	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	// 2026-10-22 01:30 in Shanghai is still the 21st in UTC.
	at := time.Date(2026, 10, 21, 17, 30, 0, 0, time.UTC)
	if d := DayOf(at, shanghai); d != 20261022 {
		t.Fatalf("day = %d", d)
	}
	start, end := Day(20261022).Bounds(shanghai)
	if time.UnixMilli(start).UTC() != time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC) || end-start != 24*3600*1000 {
		t.Fatalf("bounds = %v %v", time.UnixMilli(start).UTC(), time.UnixMilli(end).UTC())
	}
	first, last := Day(20261022).HoursOf(shanghai)
	if first != start || last != end {
		t.Fatalf("whole-hour zone hours = %d %d", first, last)
	}
	// Half an hour off: the day takes the hours that start in it.
	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	first, last = Day(20261022).HoursOf(kolkata)
	s, e := Day(20261022).Bounds(kolkata)
	if first != s+30*60*1000 || last != e+30*60*1000 || (last-first)%(3600*1000) != 0 {
		t.Fatalf("half-hour zone hours = %d %d bounds %d %d", first, last, s, e)
	}
	if Day(20261031).Next() != 20261101 || Day(20261231).Next() != 20270101 {
		t.Fatal("next day")
	}
	if HourOfMS(time.Date(2026, 10, 22, 3, 59, 59, 0, time.UTC).UnixMilli()) != time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatal("hour")
	}
}

func TestHistogramQuantilesAndEncoding(t *testing.T) {
	var h Histogram
	if _, ok := h.Quantile(0.5); ok {
		t.Fatal("empty histogram has a quantile")
	}
	for i := 0; i < 90; i++ {
		h.Observe(100)
	}
	for i := 0; i < 10; i++ {
		h.Observe(10000)
	}
	h.Observe(1e12) // beyond the last bin: kept, in the last bin
	p50, _ := h.Quantile(0.5)
	p95, _ := h.Quantile(0.95)
	if math.Abs(p50-100)/100 > 0.1 || math.Abs(p95-10000)/10000 > 0.1 {
		t.Fatalf("p50 %v p95 %v", p50, p95)
	}
	enc := h.Encode()
	if len(enc) > 12 {
		t.Fatalf("encoding of three bins is %d bytes", len(enc))
	}
	back, err := DecodeHistogram(enc)
	if err != nil || back != h || back.Count() != 101 {
		t.Fatalf("round trip: %v", err)
	}
	if empty, err := DecodeHistogram(nil); err != nil || empty.Count() != 0 {
		t.Fatal("empty decode")
	}
	for _, bad := range [][]byte{{200, 1}, {3}, {3, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}} {
		if _, err := DecodeHistogram(bad); err == nil {
			t.Fatalf("malformed %v decoded", bad)
		}
	}
	var sum Histogram
	sum.Add(h)
	sum.Add(h)
	if sum.Count() != 202 {
		t.Fatal("add")
	}
}

func request(dep uint, key uint, src uxrecord.Source, mut func(*uxrecord.Request)) uxrecord.Request {
	r := uxrecord.Request{
		ReceivedMS: time.Date(2026, 10, 22, 3, 10, 0, 0, time.UTC).UnixMilli(), DeploymentID: dep, APIKeyID: key, UserID: 1, Source: src,
		Stream: true, Status: 200, Outcome: uxrecord.OutcomeOK, FinishReason: "stop",
		FirstThinkMS: 300, FirstAnswerMS: 2000, LastChunkMS: 5000, EndMS: 5001,
		Usage: true, PromptTokens: 1000, CachedTokens: 800, CompletionTokens: 400, ReasoningTokens: uxrecord.Unknown,
	}
	if mut != nil {
		mut(&r)
	}
	return r
}

func TestRequestsCountUsageAndQualityHonestly(t *testing.T) {
	r := NewRequests()
	r.Observe(request(17, 3, uxrecord.SourceAPI, nil))
	// No cache figure: its prompt is not part of the hit rate's denominator.
	r.Observe(request(17, 3, uxrecord.SourceAPI, func(x *uxrecord.Request) { x.CachedTokens = uxrecord.Unknown; x.ReasoningTokens = 30 }))
	// No usage at all, an error.
	r.Observe(request(17, 3, uxrecord.SourceAPI, func(x *uxrecord.Request) {
		x.Usage, x.Status, x.Outcome = false, 502, uxrecord.OutcomeUpstreamError
	}))
	// Cut at its limit, and one the client left.
	r.Observe(request(17, 3, uxrecord.SourceAPI, func(x *uxrecord.Request) { x.FinishReason = uxrecord.FinishLength }))
	r.Observe(request(17, 3, uxrecord.SourceAPI, func(x *uxrecord.Request) { x.Usage, x.Outcome = false, uxrecord.OutcomeClientClosed }))
	// Another source and key are other rows.
	r.Observe(request(17, 0, uxrecord.SourcePlayground, nil))
	// Ended in the next hour: counted there.
	r.Observe(request(17, 3, uxrecord.SourceAPI, func(x *uxrecord.Request) { x.EndMS = 3600 * 1000 }))

	usage := r.TakeUsage()
	hour := time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC).UnixMilli()
	api := usage[UsageKey{HourMS: hour, APIKeyID: 3, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}]
	want := Usage{Requests: 5, NoUsage: 2, PromptTokens: 3000, CompletionTokens: 1200, ReasoningTokens: 30, ReasoningReported: 1,
		CachedTokens: 1600, CacheReported: 2, CacheReportedPrompt: 2000}
	if api != want {
		t.Fatalf("api usage = %+v\nwant %+v", api, want)
	}
	if pg := usage[UsageKey{HourMS: hour, UserID: 1, DeploymentID: 17, Source: uxrecord.SourcePlayground}]; pg.Requests != 1 || pg.PromptTokens != 1000 {
		t.Fatalf("playground = %+v", pg)
	}
	if next := usage[UsageKey{HourMS: hour + 3600*1000, APIKeyID: 3, UserID: 1, DeploymentID: 17, Source: uxrecord.SourceAPI}]; next.Requests != 1 {
		t.Fatalf("next hour = %+v (%d rows)", next, len(usage))
	}
	if len(r.TakeUsage()) != 0 {
		t.Fatal("take did not clear")
	}
	quality := r.TakeQuality()
	q := quality[QualityKey{HourMS: hour, DeploymentID: 17}]
	if q.Requests != 6 || q.Completed != 3 || q.Length != 1 || q.Error != 1 || q.ClientClosed != 1 || q.Interrupted != 0 {
		t.Fatalf("quality = %+v", q)
	}
	// TTFT and end-to-end of the four whole replies; speed of those with usage.
	if q.TTFT.Count() != 4 || q.EndToEnd.Count() != 4 || q.OutputTokens != 1600 || q.DecodeMS != 4*4700 {
		t.Fatalf("timings = ttft %d e2e %d tokens %d decode %d", q.TTFT.Count(), q.EndToEnd.Count(), q.OutputTokens, q.DecodeMS)
	}
	if p50, _ := q.TTFT.Quantile(0.5); math.Abs(p50-300)/300 > 0.1 {
		t.Fatalf("ttft p50 = %v", p50)
	}
}

func TestRequestsGiveBackAndConcurrentObserve(t *testing.T) {
	r := NewRequests()
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Observe(request(uint(g%5), 1, uxrecord.SourceAPI, nil))
			}
		}(g)
	}
	var taken []map[UsageKey]Usage
	for i := 0; i < 5; i++ {
		taken = append(taken, r.TakeUsage())
	}
	wg.Wait()
	taken = append(taken, r.TakeUsage())
	// A flush that failed gives its delta back; nothing is lost or doubled.
	r.GiveBackUsage(taken[0])
	r.GiveBackUsage(taken[1])
	taken = append(taken[2:], r.TakeUsage())
	var total int64
	for _, d := range taken {
		for _, u := range d {
			total += u.Requests
		}
	}
	if total != 16*500 {
		t.Fatalf("requests = %d", total)
	}
	q := r.TakeQuality()
	r.GiveBackQuality(q)
	var qt int64
	for _, v := range r.TakeQuality() {
		qt += v.Requests
	}
	if qt != 16*500 {
		t.Fatalf("quality requests = %d", qt)
	}
}

func TestGPUSamplesAndCardTime(t *testing.T) {
	g := NewGPUs()
	at := time.Date(2026, 10, 22, 3, 0, 10, 0, time.UTC).UnixMilli()
	card := GPUKey{NodeUUID: "n1", GPUIndex: 0, GPUUUID: "GPU-1"}
	g.Observe([]GPUSample{
		{Key: card, Metric: GPUUtilization, Value: 40, TimeMS: at},
		{Key: card, Metric: GPUUtilization, Value: 80, TimeMS: at + 10000},
		{Key: card, Metric: GPUMemoryUsed, Value: 70e9, TimeMS: at},
		{Key: card, Metric: GPUMemoryTotal, Value: 84e9, TimeMS: at},
		{Key: card, Metric: GPUPower, Value: 300, TimeMS: at},
		{Key: card, Metric: GPUTemperature, Value: 60, TimeMS: at},
		{Key: card, Metric: GPUTemperature, Value: 70, TimeMS: at + 3600*1000}, // next hour
	})
	rows := g.Take()
	key := card
	key.HourMS = HourOfMS(at)
	row := rows[key]
	if len(rows) != 2 || row.UtilSum != 120 || row.UtilN != 2 || row.UtilMax != 80 || row.MemTotal != 84e9 || row.TempMax != 60 || row.PowerN != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	g.GiveBack(rows)
	g.GiveBack(map[GPUKey]GPU{key: {UtilSum: 10, UtilN: 1, UtilMax: 95}})
	if again := g.Take()[key]; again.UtilN != 3 || again.UtilMax != 95 {
		t.Fatalf("give back = %+v", again)
	}

	times := NewGPUTimes()
	now := time.Date(2026, 10, 22, 3, 30, 0, 0, time.UTC)
	holdings := []Holding{
		{DeploymentID: 17, Cards: []HeldCard{{"RTX 6000D", 1}, {"RTX 6000D", 1}}},
		{DeploymentID: 17, Cards: []HeldCard{{"RTX 6000D", 1}, {"RTX 6000D", 1}}},
		{DeploymentID: 9, Cards: []HeldCard{{"PPU 810E", 0.25}}},
		{DeploymentID: 9},
	}
	cards := []Card{{"RTX 6000D", true}, {"RTX 6000D", true}, {"RTX 6000D", true}, {"RTX 6000D", true}, {"PPU 810E", true}, {"PPU 810E", false}}
	times.Sample(now, time.Minute, holdings, cards)
	times.Sample(now.Add(time.Minute), time.Minute, holdings[:1], cards)
	tt, cc := times.Take()
	hour := HourMS(now)
	d17 := tt[GPUTimeKey{hour, 17, "RTX 6000D"}]
	if d17.GPUSeconds != 6*60 || d17.WholeGPUSeconds != 6*60 || d17.InstanceSeconds != 3*60 || d17.MaxInstances != 2 {
		t.Fatalf("deployment 17 = %+v", d17)
	}
	if d9 := tt[GPUTimeKey{hour, 9, "PPU 810E"}]; d9.GPUSeconds != 15 || d9.WholeGPUSeconds != 60 || d9.MaxInstances != 1 {
		t.Fatalf("deployment 9 = %+v", d9)
	}
	if ppu := cc[CapacityKey{hour, "PPU 810E"}]; ppu.TotalGPUSeconds != 240 || ppu.SchedulableGPUSeconds != 120 {
		t.Fatalf("capacity = %+v", ppu)
	}
	times.GiveBack(tt, cc)
	tt2, _ := times.Take()
	if tt2[GPUTimeKey{hour, 17, "RTX 6000D"}].MaxInstances != 2 {
		t.Fatal("give back max")
	}
}

func BenchmarkRequestsObserve(b *testing.B) {
	r := NewRequests()
	recs := make([]uxrecord.Request, 64)
	for i := range recs {
		recs[i] = request(uint(i%7), uint(i%13), uxrecord.SourceAPI, nil)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			r.Observe(recs[i%len(recs)])
			i++
		}
	})
}

// A request-log row that stood for several sampled-away requests counts as
// all of them when the history is filled in.
func TestRequestsObserveWeighted(t *testing.T) {
	r := NewRequests()
	r.ObserveWeighted(request(17, 3, uxrecord.SourceAPI, nil), 4)
	r.ObserveWeighted(request(17, 3, uxrecord.SourceAPI, nil), 0) // counts once
	var u Usage
	for _, v := range r.TakeUsage() {
		u.Add(v)
	}
	if u.Requests != 5 || u.PromptTokens != 5000 || u.CachedTokens != 4000 || u.CacheReportedPrompt != 5000 {
		t.Fatalf("usage = %+v", u)
	}
	var q Quality
	for _, v := range r.TakeQuality() {
		q.Add(v)
	}
	if q.Requests != 5 || q.Completed != 5 || q.TTFT.Count() != 5 || q.OutputTokens != 2000 {
		t.Fatalf("quality = %+v", q)
	}
}
