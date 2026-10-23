package analytics

import (
	"context"
	"sort"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
)

// GPUTime answers the card time over a range, of one deployment when
// deploymentID is not 0, with each deployment's tokens over the same range
// for the GPU hours per million tokens.
func (s *Service) GPUTime(ctx context.Context, q Query, deploymentID uint) (dto.GPUTimeAnalytics, error) {
	sp, err := s.resolve(q)
	if err != nil {
		return dto.GPUTimeAnalytics{}, err
	}
	parts, keys, err := s.statsParts(ctx, sp)
	if err != nil {
		return dto.GPUTimeAnalytics{}, err
	}
	type depModel struct {
		dep   uint
		model string
	}
	rows := map[depModel]*rollup.GPUTime{}
	series := map[uint][]float64{}
	capacity := map[string]*rollup.Capacity{}
	allocated := map[string]float64{}
	for i, part := range parts {
		times, err := s.Stats.GPUTime(ctx, part, deploymentID)
		if err != nil {
			return dto.GPUTimeAnalytics{}, err
		}
		for _, t := range times {
			k := depModel{t.DeploymentID, t.GPUModel}
			cur := rows[k]
			if cur == nil {
				cur = &rollup.GPUTime{}
				rows[k] = cur
			}
			cur.Add(t.GPUTime)
			allocated[t.GPUModel] += t.GPUSeconds
			key := t.Bucket
			if keys[i] >= 0 {
				key = keys[i]
			}
			if at := sp.position(key); at >= 0 {
				if series[t.DeploymentID] == nil {
					series[t.DeploymentID] = make([]float64, len(sp.buckets))
				}
				series[t.DeploymentID][at] += t.GPUSeconds / 3600
			}
		}
		caps, err := s.Stats.Capacity(ctx, part)
		if err != nil {
			return dto.GPUTimeAnalytics{}, err
		}
		for _, c := range caps {
			cur := capacity[c.GPUModel]
			if cur == nil {
				cur = &rollup.Capacity{}
				capacity[c.GPUModel] = cur
			}
			cur.Add(c.Capacity)
		}
	}
	// The deployments' tokens over the same span.
	tokens, err := s.usageByDeployment(ctx, sp, UsageFilter{DeploymentID: deploymentID})
	if err != nil {
		return dto.GPUTimeAnalytics{}, err
	}
	ids := map[uint]bool{}
	for k := range rows {
		ids[k.dep] = true
	}
	names, err := s.deploymentNames(ids)
	if err != nil {
		return dto.GPUTimeAnalytics{}, err
	}
	// Mean replicas over the part of the span that has passed.
	end := min(sp.toMS, s.now().UnixMilli())
	seconds := float64(max(end-sp.fromMS, 1)) / 1000
	out := dto.GPUTimeAnalytics{Rows: []dto.GPUTimeRow{}, Series: []dto.GPUTimeSeries{}, Capacity: []dto.GPUCapacity{}}
	for k, t := range rows {
		n := names[k.dep]
		tok := tokens[k.dep]
		row := dto.GPUTimeRow{DeploymentID: k.dep, DeploymentName: n.Name, ServedName: n.ServedName, DeploymentDeleted: n.Deleted,
			GPUModel: k.model, GPUHours: t.GPUSeconds / 3600, WholeGPUHours: t.WholeGPUSeconds / 3600,
			AvgInstances: t.InstanceSeconds / seconds, MaxInstances: t.MaxInstances,
			PromptTokens: tok.PromptTokens, CompletionTokens: tok.CompletionTokens}
		if tok.CompletionTokens > 0 {
			row.GPUHoursPerMillionOutput = ptr(row.GPUHours / (float64(tok.CompletionTokens) / 1e6))
		}
		if all := tok.CompletionTokens + tok.PromptTokens; all > 0 {
			row.GPUHoursPerMillionAllTokens = ptr(row.GPUHours / (float64(all) / 1e6))
		}
		out.Rows = append(out.Rows, row)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].GPUHours != out.Rows[j].GPUHours {
			return out.Rows[i].GPUHours > out.Rows[j].GPUHours
		}
		return out.Rows[i].DeploymentID < out.Rows[j].DeploymentID
	})
	for dep, hours := range series {
		out.Series = append(out.Series, dto.GPUTimeSeries{DeploymentID: dep, DeploymentName: names[dep].Name, GPUHours: hours})
	}
	sort.Slice(out.Series, func(i, j int) bool { return out.Series[i].DeploymentID < out.Series[j].DeploymentID })
	for model, c := range capacity {
		out.Capacity = append(out.Capacity, dto.GPUCapacity{GPUModel: model, TotalGPUHours: c.TotalGPUSeconds / 3600,
			SchedulableHours: c.SchedulableGPUSeconds / 3600, AllocatedGPUHours: allocated[model] / 3600})
	}
	sort.Slice(out.Capacity, func(i, j int) bool { return out.Capacity[i].GPUModel < out.Capacity[j].GPUModel })
	earliest, err := s.earliest(ctx, sp, statsdb.KindGPUTime)
	if err != nil {
		return out, err
	}
	out.Range = sp.rangeDTO(s.Retention().TimeZone, earliest, s.LiveSinceMS)
	return out, nil
}
