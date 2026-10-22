package inferencemetrics

// derivation computes one metric from others an engine reports instead. It
// runs on the samples of one scrape, and only when the engine did not report
// the target itself.
type derivation struct {
	metric  string
	inputs  []string
	compute func(inputs []float64) (float64, bool)
}

var derivations = []derivation{
	// An engine that reports its KV pool's size and free space (TensorFold's
	// tensorfold_health:pool_tokens / pool_free_tokens) gets the same usage
	// ratio vLLM reports directly, so every panel and the gateway's load
	// signal read one series.
	{
		metric: "gpu_cache_usage_percent",
		inputs: []string{"kv_capacity_tokens", "kv_free_tokens"},
		compute: func(in []float64) (float64, bool) {
			capacity, free := in[0], in[1]
			if capacity <= 0 || free < 0 || free > capacity {
				return 0, false
			}
			return (capacity - free) / capacity, true
		},
	},
}

// Derive returns the derived series values a scrape is missing. values maps
// stored series names (Definition.Series) to the scrape's values for the
// unlabeled series; the result uses the same keys.
func Derive(values map[string]float64) map[string]float64 {
	var out map[string]float64
	for _, d := range derivations {
		target := SeriesPrefix + d.metric
		if _, reported := values[target]; reported {
			continue
		}
		inputs := make([]float64, len(d.inputs))
		complete := true
		for i, name := range d.inputs {
			value, ok := values[SeriesPrefix+name]
			if !ok {
				complete = false
				break
			}
			inputs[i] = value
		}
		if !complete {
			continue
		}
		if value, ok := d.compute(inputs); ok {
			if out == nil {
				out = map[string]float64{}
			}
			out[target] = value
		}
	}
	return out
}
