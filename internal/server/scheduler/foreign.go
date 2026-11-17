package scheduler

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/accelerator"
	serverestimate "gpuworks.ai/gpuworks/internal/server/estimate"
)

// Sharing a card with processes outside the platform (ADR 0068).
//
// A card somebody else's workload holds takes no whole-card work: it is not
// free. A memory-limited partition asks for less than a card, though, and
// when the agent says how much the outside processes hold, what they leave
// - less a headroom, since their figure is a reading rather than a limit -
// is room the partition can be admitted against like any other claim.

// takesWorkFor is whether the card may host this request at all: a card
// that takes work, or one held outside the platform that a partition may
// share by memory. A deployment that sleeps is not placed beside such
// processes: its wake would be refused for as long as they hold the card.
func (g GPU) takesWorkFor(req Request) bool {
	if g.Schedulable {
		return true
	}
	return g.sharesForeignWith(req) && !req.sleeps()
}

// sharesForeignWith is whether this is a held card a partition request may
// share by memory, before the sleep rule.
func (g GPU) sharesForeignWith(req Request) bool {
	return g.ForeignShareable && req.PerGPULimitBytes > 0
}

// foreignHold is the memory a partition may not claim because processes
// outside the platform hold it: their figure and the headroom kept beside it.
func (g GPU) foreignHold() int64 {
	if g.ForeignBytes <= 0 {
		return 0
	}
	return g.ForeignBytes + serverestimate.ForeignHeadroomBytes(g.MemoryTotal)
}

// sleeps is whether the deployment puts its engine to sleep.
func (r Request) sleeps() bool {
	return slices.Contains(r.Features, accelerator.FeatureEngineSleep)
}

// pinnedGPUs narrows the filters to the cards a pin names, so a verdict
// about "the node's cards" is about the ones the request may use. A pin
// that names none of the node's cards keeps them all: the planner then
// reports the count it could not meet.
func pinnedGPUs(gpus []GPU, req Request) []GPU {
	if len(req.AllowedGPUIndexes) == 0 {
		return gpus
	}
	if pinned := allowedGPUs(gpus, req.AllowedGPUIndexes); len(pinned) > 0 {
		return pinned
	}
	return gpus
}

// heldReason says why a card held outside the platform cannot host this
// request: the sleep rule when only that stands in the way, otherwise the
// holder.
func heldReason(req Request, g GPU) string {
	if g.sharesForeignWith(req) && req.sleeps() {
		return ReasonHeldOutsidePlatformSleep
	}
	return ReasonGPUsHeldOutsidePlatform
}
