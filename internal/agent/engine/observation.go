package engine

import "gpuworks.ai/gpuworks/internal/proto"

// UpdateObservations swaps how instances this agent already holds are
// observed (StateUpdate.Observations). Nothing else of their desired state
// changes and ensure does not run: re-adopting a running container would
// re-probe its health, may report it starting and can reclaim it, for a
// change that touches nothing the container does. The collector reads the
// desired state on every tick, so the new observation applies to the next
// scrape. An update for an instance this agent does not hold is dropped: the
// instance's stored desired state already carries it for the next launch or
// state.sync, and adopting it here would start an instance nobody asked for.
func (e *Engine) UpdateObservations(updates []proto.ObservationUpdate) {
	if len(updates) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, update := range updates {
		spec, held := e.desired[update.UUID]
		if !held {
			continue
		}
		spec.Observation = update.Observation
		spec.MetricPath, spec.MetricPrefix, spec.MetricMap = update.MetricPath, update.MetricPrefix, update.MetricMap
		e.desired[update.UUID] = spec
	}
}
