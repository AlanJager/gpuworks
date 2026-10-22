package monitorapi

import (
	"context"
	"fmt"
	"slices"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Latency serves GET /monitor/deployments/{id}/latency: the deployment's
// latency breakdown over the range ending now (uxrecord.LatencyOf). The
// means come from the sums, the percentiles from the segment histograms,
// both kept for every whole reply whether the request log sampled it or not.
func (s *Service) Latency(ctx context.Context, id uint, rng time.Duration) (dto.MonitorLatency, error) {
	src := s.deps.Requests
	if src == nil || !src.DeploymentExists(id) {
		return dto.MonitorLatency{}, s.deps.NotFound
	}
	now := s.deps.now()
	out := dto.MonitorLatency{FromMS: now.Add(-rng).UnixMilli(), ToMS: now.UnixMilli(), Segments: []dto.MonitorLatencySegment{}}
	sums, err := src.Latency(ctx, id, out.FromMS, out.ToMS)
	if err != nil {
		return dto.MonitorLatency{}, fmt.Errorf("latency sums: %w", err)
	}
	cells, err := src.Grid(ctx, id, uxrecord.GridSegments, out.FromMS, out.ToMS)
	if err != nil {
		return dto.MonitorLatency{}, fmt.Errorf("latency histograms: %w", err)
	}
	out.Requests = sums.Requests
	if sums.Requests == 0 {
		return out, nil
	}
	rows := map[int][]uxrecord.Cell{}
	for _, cell := range cells {
		rows[cell.X] = append(rows[cell.X], cell)
	}
	n := float64(sums.Requests)
	e2e := rows[uxrecord.LatencyRow(uxrecord.LatencyEndToEnd)]
	_, p50, p95 := percentiles(e2e)
	out.EndToEnd = &dto.MonitorLatencyStat{MeanMS: sums.EndToEndMS / n, P50MS: p50, P95MS: p95}
	for _, kind := range uxrecord.SegmentKinds() {
		total := sums.SegmentMS[kind]
		if total <= 0 {
			continue
		}
		count, p50, p95 := percentiles(rows[uxrecord.LatencyRow(string(kind))])
		out.Segments = append(out.Segments, dto.MonitorLatencySegment{
			Kind: string(kind), MeanMS: total / n, Requests: count, P50MS: p50, P95MS: p95,
		})
	}
	out.Speeds = dto.MonitorLatencySpeeds{
		PrefillTokS:   tokensPerSecond(sums.PrefillTokens, sums.PrefillMS),
		ThinkingTokS:  tokensPerSecond(sums.ThinkingTokens, sums.ThinkingMS),
		AnsweringTokS: tokensPerSecond(sums.AnsweringTokens, sums.AnsweringMS),
		DecodeTokS:    tokensPerSecond(sums.DecodeTokens, sums.DecodeMS),
	}
	return out, nil
}

// percentiles reads a histogram row: how many requests it holds and the
// representative durations of the bins holding its 50th and 95th
// percentiles.
func percentiles(row []uxrecord.Cell) (count int64, p50, p95 float64) {
	slices.SortFunc(row, func(a, b uxrecord.Cell) int { return a.Y - b.Y })
	for _, cell := range row {
		count += cell.N
	}
	if count == 0 {
		return 0, 0, 0
	}
	at := func(q float64) float64 {
		need := q * float64(count)
		var seen int64
		for _, cell := range row {
			seen += cell.N
			if float64(seen) >= need {
				return uxrecord.LatencyBinMS(cell.Y)
			}
		}
		return uxrecord.LatencyBinMS(row[len(row)-1].Y)
	}
	return count, at(0.5), at(0.95)
}

func tokensPerSecond(tokens, ms float64) *float64 {
	if tokens <= 0 || ms <= 0 {
		return nil
	}
	v := tokens * 1000 / ms
	return &v
}
