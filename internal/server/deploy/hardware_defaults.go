package deploy

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/estimate"
)

// CardFacts is what a default needs to know about one accelerator the
// deployment may run on. It carries the stored facts rather than a conclusion:
// whether the card's memory is the host's pool follows from its model name and
// is derived here, so a caller cannot pass a card whose capacity and memory
// topology disagree.
type CardFacts struct {
	// Name is the model as the server stores it (accelerator.NormalizeGPUName).
	Name string
	// MemoryTotalBytes is the capacity the node reported; zero means the card
	// was never measured, and a default must then stay as the dictionary left
	// it rather than be derived from nothing.
	MemoryTotalBytes int64
}

// capDefaultsToCards lowers the dictionary defaults a card can make unsafe and
// returns the dictionary to read for this deployment. The rows stay as they
// were for every other reader: the map it is given is shared across the
// process, so the adjusted one is a copy.
//
// Today one row is capped. gpu_memory_utilization is a share of the card, so a
// constant leaves less and less behind as cards get smaller, while what
// competes for the remainder - the driver context, a display output, another
// process, the host OS of a unified-memory part - does not shrink with them.
//
// Where the rule's parts live, because no one package owns all of it:
//
//   - accelerator.UnifiedMemory says whether a card's memory is the host's.
//   - estimate.MaxGPUMemUtil holds the reserve and turns a capacity into the
//     highest share that still leaves it.
//   - here: which row it applies to, which card decides, and the row's bounds.
//   - api.deployTargetCards: which cards a request is measured against.
//
// Only the lowest answer is used: one share is applied to every card the
// deployment runs on, so the smallest card binds. A deployment whose cards are
// not pinned is therefore capped by the smallest card it could land on - never
// under-reserving, but leaving more than necessary on the larger cards of a
// mixed node. Pinning the GPUs removes both the ambiguity and the extra
// reserve, and a deployer who wants the memory back can state the share, which
// this never overrides.
func capDefaultsToCards(params map[string]engineParam, cards []CardFacts) map[string]engineParam {
	const key = "gpu_memory_utilization"
	spec, known := params[key]
	if !known || spec.Step == nil {
		return params
	}
	current, isNumber := toFloat(spec.Default)
	if !isNumber {
		return params
	}
	capped, ok := lowestShareTheCardsAllow(cards, *spec.Step)
	if !ok || capped >= current {
		return params
	}
	if spec.Min != nil && capped < *spec.Min {
		// A card that cannot hold the reserve at all has no share to offer.
		// The floor keeps the row inside its own published bounds and leaves
		// the refusal to admission, which can say the card is too small.
		capped = *spec.Min
	}
	spec.Default = capped
	out := make(map[string]engineParam, len(params))
	for name, row := range params {
		out[name] = row
	}
	out[key] = spec
	return out
}

// lowestShareTheCardsAllow is the strictest share these cards permit, and
// whether any of them reported a capacity to derive one from. A card nobody
// measured is skipped rather than counted as small.
func lowestShareTheCardsAllow(cards []CardFacts, step float64) (float64, bool) {
	lowest, found := 0.0, false
	for _, card := range cards {
		share, ok := estimate.MaxGPUMemUtil(card.MemoryTotalBytes, accelerator.UnifiedMemory(card.Name), step)
		if !ok {
			continue
		}
		if !found || share < lowest {
			lowest, found = share, true
		}
	}
	return lowest, found
}
