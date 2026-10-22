package search

// BestMeasured selects the highest declared objective among measured points
// satisfying the same constraints used by the adaptive search. It makes no
// claim about unmeasured loads or a stable capacity boundary.
func BestMeasured(observations []Observation, constraints SLAConfig) (Observation, bool) {
	var best Observation
	found := false
	for _, observation := range observations {
		if objectiveValue(observation, constraints.Objective) <= 0 || len(constraints.Violations(observation)) != 0 {
			continue
		}
		if !found || betterObservation(observation, best, constraints.Objective) {
			best, found = observation, true
		}
	}
	return best, found
}

func betterObservation(candidate, current Observation, objective string) bool {
	left, right := objectiveValue(candidate, objective), objectiveValue(current, objective)
	if left != right {
		return left > right
	}
	if candidate.Point.RequestRate != current.Point.RequestRate {
		return candidate.Point.RequestRate < current.Point.RequestRate
	}
	return candidate.Point.Concurrency < current.Point.Concurrency
}

// Old observations did not retain sample counts. Nonzero historical timings
// remain usable with unknown n, preserving their SLA interpretation. A zero
// without an explicit positive sample count is not evidence of zero latency.
func metricAvailable(o Observation, metric string, actual float64) bool {
	sample := -1
	switch metric {
	case "avg_ttft", "p99_ttft":
		if o.Samples != nil {
			sample = o.Samples.TTFT
		}
	case "avg_tpot", "p99_tpot", "interactivity":
		if o.Samples != nil {
			sample = o.Samples.TPOT
		}
	case "avg_latency", "p99_latency":
		if o.Samples != nil {
			sample = o.Samples.Latency
		}
	default:
		return true
	}
	if sample >= 0 {
		return sample > 0
	}
	return actual > 0
}
