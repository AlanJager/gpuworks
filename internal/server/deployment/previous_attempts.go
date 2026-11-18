package deployment

import "gpuworks.ai/gpuworks/internal/server/planning"

// PreviousAttempts identifies retained failures that no longer represent a
// missing replica. Interchangeable replicas share a retry budget: while any
// slot of that class is unfilled, its newest failure remains current, as in
// the unit's last-failure and retry-exhaustion response. Earlier failures are
// history even after every retry failed.
func PreviousAttempts(match planning.UnitMatch) map[string]bool {
	unfilled := map[string]bool{}
	for _, slot := range match.Slots {
		if slot.Inst == nil {
			unfilled[slot.Key] = true
		}
	}
	previous := map[string]bool{}
	for key, attempts := range match.Failed {
		if len(attempts) == 0 {
			continue
		}
		newest := planning.NewestAttempt(attempts)
		for _, inst := range attempts {
			if !unfilled[key] || inst.UUID != newest.UUID {
				previous[inst.UUID] = true
			}
		}
	}
	return previous
}
