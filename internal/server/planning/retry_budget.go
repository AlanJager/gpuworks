package planning

import (
	"sort"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// FreePortRefusals is how many launches refused for a taken host port
// (proto.EnginePortRefused) a unit gets without charging its retry budget.
// Such a launch started nothing, and each retry avoids the ports every
// earlier attempt was given (their rows keep them reserved), so a few in a
// row are expected on a host where other processes listen in the engine port
// range; the cap stops a host that keeps refusing from being retried forever
// (docs/adr/0029).
const FreePortRefusals = 10

// ChargedAttempts is the part of a unit's failed attempts its retry budget
// counts: all of them except the first FreePortRefusals port refusals, the
// oldest first. The rows themselves all stay; only what the budget charges
// differs.
func ChargedAttempts(attempts []store.Instance) []store.Instance {
	ordered := append([]store.Instance(nil), attempts...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].CreatedAt.Before(ordered[j].CreatedAt) })
	charged := make([]store.Instance, 0, len(ordered))
	free := 0
	for _, inst := range ordered {
		if proto.IsEnginePortRefusal(inst.Message) && free < FreePortRefusals {
			free++
			continue
		}
		charged = append(charged, inst)
	}
	return charged
}

// RetryAtOnce says a unit's newest failed attempt was a port refusal the
// budget does not charge: nothing started, so the retry need not back off.
func RetryAtOnce(attempts []store.Instance) bool {
	if len(attempts) == 0 {
		return false
	}
	newest := NewestAttempt(attempts)
	if !proto.IsEnginePortRefusal(newest.Message) {
		return false
	}
	for _, inst := range ChargedAttempts(attempts) {
		if inst.UUID == newest.UUID {
			return false
		}
	}
	return true
}
