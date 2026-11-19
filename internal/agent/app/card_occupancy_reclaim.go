package app

import (
	"log/slog"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The engines of instances that have just left a card.
//
// An instance holds its cards until its container is confirmed gone
// (engine.HeldCards). What the vendor tool lists can still name its engine
// a moment later: the list was read while the container was being removed,
// or a process the runtime killed is still letting go of the card. With no
// instance of ours left there the card is examined, and that process is
// reported like any other the platform does not plan around - the card
// must not take work while its memory is held.
//
// The list may also be older than the teardown: a slow vendor tool is
// answered from its last result. A card our instances have just left is
// examined only by a list produced after a reading first saw it without
// them (noteHeld, listPredatesLeaving); until then its last answer stands.
//
// It is not somebody else's job, though, and the release rounds are for
// those: a card whose only holders were our own engines on their way out
// is let go by the first reading that finds them gone, not the third.

// noteEngines records the processes listed on a card that is ours and not
// examined: every one of them could be our engine's, so the reading after
// our instances leave knows them (leavingEngines).
func (o *cardOccupancy) noteEngines(ident string, procs []gpu.ComputeProcess, bdf string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	listed := map[int]string{}
	for _, proc := range procs {
		if gpu.SameBDF(proc.PCIeBDF, bdf) {
			listed[proc.PID] = proc.Name
		}
	}
	if len(listed) == 0 {
		delete(o.engines, ident)
		return
	}
	if o.engines == nil {
		o.engines = map[string]map[int]string{}
	}
	o.engines[ident] = listed
}

// leavingEngines is, on a card none of our instances holds any more, the
// processes that could be our engines' while they did: the ones listed on
// it then (engines), or taken for theirs on a card read as shared (own).
// Those still listed under the same PID and name stay recorded for the next
// reading.
func (o *cardOccupancy) leavingEngines(ident string, procs []gpu.ComputeProcess, bdf string) map[int]string {
	o.mu.Lock()
	defer o.mu.Unlock()
	were := map[int]string{}
	for pid, name := range o.engines[ident] {
		were[pid] = name
	}
	for pid, name := range o.own[ident] {
		were[pid] = name
	}
	still := map[int]string{}
	for _, proc := range procs {
		if name, ok := were[proc.PID]; ok && name == proc.Name && gpu.SameBDF(proc.PCIeBDF, bdf) {
			still[proc.PID] = name
		}
	}
	if len(still) == 0 {
		delete(o.engines, ident)
	} else {
		if o.engines == nil {
			o.engines = map[string]map[int]string{}
		}
		o.engines[ident] = still
	}
	return were
}

// releaseLeft lets a card go at once when it was found free and every
// process of its last answer was one of our own engines leaving (engines).
// A card anybody else held as well keeps its release rounds (remember).
func (o *cardOccupancy) releaseLeft(ident string, engines map[int]string, bdf string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	held := o.last[ident]
	if len(held) == 0 || !onlyEngines(held, engines) {
		return
	}
	// remember writes the file: the card is absent from its next answer.
	delete(o.last, ident)
	delete(o.freeRounds, ident)
	slog.Info("card let go by the platform's own engine", "card", bdf)
}

// onlyEngines is whether every process is one of engines, by PID and name.
func onlyEngines(procs []proto.CardProcess, engines map[int]string) bool {
	for _, proc := range procs {
		if name, ok := engines[proc.PID]; !ok || proc.PID <= 0 || name != proc.Name {
			return false
		}
	}
	return true
}

// noteHeld records, at a reading asked at now, which cards our instances
// hold, and for a card they held at the last reading and no longer do, that
// this reading was the first to find them gone.
func (o *cardOccupancy) noteHeld(ours map[string]int, now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.held == nil {
		o.held = map[string]time.Time{}
	}
	for ident, left := range o.held {
		if ours[ident] == 0 && left.IsZero() {
			o.held[ident] = now
		}
	}
	for ident, slots := range ours {
		if slots > 0 {
			o.held[ident] = time.Time{}
		}
	}
}

// listPredatesLeaving is whether a process list produced at listed is older
// than the reading that first found our instances gone from the card: it
// may still name their engines. A list that does not is the one that
// examines the card, and the card is like any other from then on.
func (o *cardOccupancy) listPredatesLeaving(ident string, listed time.Time) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	left, ok := o.held[ident]
	if !ok {
		return false
	}
	if left.IsZero() || listed.Before(left) {
		return true
	}
	delete(o.held, ident)
	return false
}
