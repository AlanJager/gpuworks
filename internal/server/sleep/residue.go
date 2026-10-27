package sleep

// What a sleeping vLLM instance still holds on each of its GPUs after it has
// offloaded (L1) or discarded (L2) its weights and KV cache: the CUDA
// context, allocator pools, cuBLAS workspace, the CUDA graph memory pool and,
// with more than one GPU, the collective's communication buffers. Measured
// residues so far: 561 MiB (eager) and 995 MiB (CUDA graphs) for a 0.5B model
// on one GPU, and about 4 GB per GPU for DeepSeek-V4-Flash on 4×H200 (2026-08
// and 2026-09 sleep evaluations).

const (
	mib int64 = 1 << 20
	gib int64 = 1 << 30

	// UnknownResidueBytes is charged for a sleeper whose model size is not
	// known: the per-GPU figure the ledger charged every sleeper before
	// residues were estimated per instance.
	UnknownResidueBytes = 3 * gib / 2

	minResidueBytes = 3 * gib / 4
	maxResidueBytes = 4 * gib
)

// EstimateResidueBytes is the per-GPU residue a sleeper is charged when no
// measurement of its own is at hand. The base grows with the model (a larger
// hidden size means larger activation workspaces and graph pools); CUDA graph
// capture adds its pool, assumed on because the stored launch does not say
// whether the engine runs eager; and a group of more than one GPU adds the
// collective's buffers, more again from eight GPUs. The result stays between
// 0.75 GiB and 4 GiB. params is the model's total parameter count, 0 when
// unknown; gpus is how many GPUs the engine spans across all its nodes.
func EstimateResidueBytes(params float64, gpus int) int64 {
	if params <= 0 {
		return UnknownResidueBytes
	}
	var bytes int64
	switch {
	case params <= 3e9:
		bytes = 3 * gib / 4
	case params <= 30e9:
		bytes = gib
	case params <= 200e9:
		bytes = 3 * gib / 2
	default:
		bytes = 5 * gib / 2
	}
	bytes += gib / 2 // CUDA graph pool
	switch {
	case gpus >= 8:
		bytes += gib
	case gpus > 1:
		bytes += gib / 2
	}
	return min(max(bytes, minResidueBytes), maxResidueBytes)
}

// ResidueBytes is what a sleeper is charged per GPU: its measured residue
// when the agent could measure one (the instance alone on its cards, read
// from the card's used memory once the sleep returned), else the estimate.
// A card reading includes the driver's own small share, so it errs high,
// which is the safe direction.
func ResidueBytes(measured, estimated int64) int64 {
	if measured <= 0 {
		return estimated
	}
	return measured
}
