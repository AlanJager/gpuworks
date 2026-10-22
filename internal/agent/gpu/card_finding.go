package gpu

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
)

// SetCardFinding records one health finding on a card: the code, the level
// that code puts the card at, the evidence as text, and the Healthy flag.
//
// They move together or not at all. Setting three of them and forgetting
// the fourth leaves a card that reads healthy and unschedulable at once,
// and every reader then picks a different one of the two to believe. The
// probers and the agent's own checks both come through here, so a new
// finding is a new code rather than a fourth hand-written copy of this.
func SetCardFinding(card *proto.GPUInfo, code string, evidence []accelerator.HealthField) {
	card.HealthCode = code
	card.HealthStatus = accelerator.HealthLevel(code)
	card.HealthMessage = accelerator.FormatHealthEvidence(evidence)
	card.Healthy = CardIsHealthy(*card)
}

// CardIsHealthy is the flag this agent reports for a card, from the one
// definition both sides share (accelerator.CardHealthy): a card at a level
// that may be scheduled, which this node owns. A card lent to a virtual
// machine is not faulty and still is not healthy — "this card cannot
// serve" is equally true of it.
func CardIsHealthy(card proto.GPUInfo) bool {
	return accelerator.CardHealthy(card.Ownership, card.HealthStatus)
}
