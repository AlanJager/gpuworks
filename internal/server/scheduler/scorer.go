package scheduler

// scorePlans applies the scorers and returns the highest-scoring plan.
// Score = topology (already on each plan from the planner) + packing (prefer
// fewer GPUs / smaller tp to reduce fragmentation) + tight-fit bonus.
// A placement contract's Preference is compared before the score: the highest
// preference wins outright and the score only breaks ties, so a contract that
// states none (every Preference 0) is scored exactly as before.
// Locality (+50 model already on node) and Eviction (-40/instance) are future
// scorers that hook in here; today the score is topology + packing.
func scorePlans(plans []Plan, node NodeSnapshot) *Plan {
	var best *Plan
	for i := range plans {
		p := &plans[i]

		// Packing: each extra GPU is a real cost (fragmentation), so penalize
		// larger tp strongly — enough that a topology bonus can differentiate
		// between multi-GPU options of the same tp but never make multi-GPU beat
		// a single GPU that already fits.
		packScore := -50 * (p.TP - 1)
		p.Reasons = append(p.Reasons, Reason{
			Code: ReasonPacking, Score: packScore,
		})

		// Tight-fit: reward using GPUs whose capacity is closest to the need
		// (avoid stranding a huge GPU on a tiny model).
		fit := tightFitScore(p, node)
		if fit != 0 {
			p.Reasons = append(p.Reasons, Reason{Code: ReasonTightFit, Score: fit})
		}

		total := 0
		for _, r := range p.Reasons {
			total += r.Score
		}
		p.Score = total

		if best == nil || p.Preference > best.Preference || (p.Preference == best.Preference && p.Score > best.Score) {
			best = p
		}
	}
	return best
}

// tightFitScore gives a small bonus when the chosen GPUs are not massively
// oversized for the requirement (0..10).
func tightFitScore(p *Plan, node NodeSnapshot) int {
	memByIdx := map[int]int64{}
	for _, g := range node.GPUs {
		memByIdx[g.Index] = g.MemoryTotal
	}
	var minCap int64 = 1 << 62
	for _, gi := range p.GPUIndexes {
		if m := memByIdx[gi]; m > 0 && m < minCap {
			minCap = m
		}
	}
	if minCap <= 0 || p.PerGPUBytes <= 0 {
		return 0
	}
	util := float64(p.PerGPUBytes) / float64(minCap)
	switch {
	case util >= 0.7:
		return 10
	case util >= 0.5:
		return 6
	case util >= 0.3:
		return 3
	default:
		return 0
	}
}

// highlight builds the topology highlight for the chosen plan.
func highlight(p *Plan, node NodeSnapshot) TopoHighlight {
	h := TopoHighlight{GPUs: p.GPUIndexes}
	numaSet := map[int]bool{}
	for _, g := range node.GPUs {
		for _, gi := range p.GPUIndexes {
			if g.Index == gi && g.NUMANode >= 0 {
				numaSet[g.NUMANode] = true
			}
		}
	}
	for n := range numaSet {
		h.NUMA = append(h.NUMA, n)
	}
	// Links: every pair among the chosen GPUs (the UI draws the interconnect).
	for i := 0; i < len(p.GPUIndexes); i++ {
		for j := i + 1; j < len(p.GPUIndexes); j++ {
			h.Links = append(h.Links, [2]int{p.GPUIndexes[i], p.GPUIndexes[j]})
		}
	}
	return h
}
