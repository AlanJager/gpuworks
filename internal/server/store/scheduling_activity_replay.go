package store

import (
	"encoding/json"
	"sort"
	"time"

	"gorm.io/gorm"
)

// ActivityReplay is how the scheduling activity is filled from the event log
// once, when the server starts: every event it counts from Since up to
// ThroughID, as the store would have handed it over (ObservedEvent).
type ActivityReplay struct {
	Since     time.Time
	ThroughID uint
	// Scheduling takes a page of scheduling events, in id order.
	Scheduling func([]ObservedEvent)
	// Residency takes the state, sleep and removal events of a group of
	// deployments' instances, in the order they happened: the newest before
	// Since (dated Since) and those after it. An instance no event describes
	// is given its row's state, dated when it began to count.
	Residency func([]ObservedEvent)
}

// activityReplayPage bounds the scheduling events one read returns.
const activityReplayPage = 5000

// activityReplayGroup bounds the deployments whose residency one read
// covers, so the replay holds a group's events at a time, not the window's.
const activityReplayGroup = 50

// LastEventID is the id of the newest event, 0 when there is none.
func (s *Store) LastEventID() (uint, error) {
	var id uint
	err := s.reader().Model(&InstanceEvent{}).Select("COALESCE(MAX(id), 0)").Scan(&id).Error
	return id, err
}

// ReplaySchedulingActivity reads back what the scheduling activity counts
// over [r.Since, r.ThroughID], by index ranges only: the scheduling events by
// (kind, id) from the window's first id, in pages; the cards they concern
// from the instances still placed and the removals of the window; then the
// residency of the deployments that share cards (SharedDeploymentIDs, and
// those a yield of the window names) a group at a time (ResidencyInputsOf);
// last, every other instance's row, dated now, so that the activity knows
// what each instance is doing from now on.
func (s *Store) ReplaySchedulingActivity(r ActivityReplay, now time.Time) error {
	firstID, haveWindow, err := s.ResidencyWindowStart(r.Since)
	if err != nil {
		return err
	}
	scope := map[uint]bool{}
	if haveWindow && firstID <= r.ThroughID {
		removed, err := s.removedPlacements(firstID, r.ThroughID)
		if err != nil {
			return err
		}
		if err := s.replayScheduling(r, firstID, removed, scope); err != nil {
			return err
		}
	}
	shared, err := s.SharedDeploymentIDs()
	if err != nil {
		return err
	}
	for _, id := range shared {
		scope[id] = true
	}
	ids := make([]uint, 0, len(scope))
	for id := range scope {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	replayed := map[string]bool{}
	for start := 0; start < len(ids); start += activityReplayGroup {
		end := min(start+activityReplayGroup, len(ids))
		if err := s.replayResidency(r, ids[start:end], replayed); err != nil {
			return err
		}
	}
	return s.replayLiveRows(r, replayed, now)
}

// removedPlacements is where the instances removed in the window sat, from
// their INSTANCE_REMOVED events: the cards of an event naming an instance
// gone since.
func (s *Store) removedPlacements(firstID, throughID uint) (map[string]instancePlacement, error) {
	var removed []InstanceEvent
	if err := residencyRemovedQuery(s.reader(), firstID).Where("id <= ?", throughID).Find(&removed).Error; err != nil {
		return nil, err
	}
	nodeIDs := make([]uint, 0, len(removed))
	for _, row := range removed {
		nodeIDs = append(nodeIDs, decodeResidencyEvent(row).NodeID)
	}
	refs, err := s.NodeRefsByID(nodeIDs)
	if err != nil {
		return nil, err
	}
	out := map[string]instancePlacement{}
	for _, row := range removed {
		ev := decodeResidencyEvent(row)
		out[row.InstanceUUID] = instancePlacement{nodeUUID: refs[ev.NodeID].UUID, gpus: ev.GPUs}
	}
	return out, nil
}

// replayScheduling hands over the window's scheduling events kind by kind,
// a page at a time, and adds the deployments its yields name to scope.
func (s *Store) replayScheduling(r ActivityReplay, firstID uint, removed map[string]instancePlacement, scope map[uint]bool) error {
	byKind := map[string][]string{}
	var kinds []string
	for _, entry := range schedulingEventCodes {
		if byKind[entry.Kind] == nil {
			kinds = append(kinds, entry.Kind)
		}
		byKind[entry.Kind] = append(byKind[entry.Kind], entry.Code)
	}
	for _, kind := range kinds {
		after := firstID
		for {
			var rows []InstanceEvent
			if err := activityReplayQuery(s.reader(), kind, byKind[kind], after, r.ThroughID).Find(&rows).Error; err != nil {
				return err
			}
			if len(rows) == 0 {
				break
			}
			page, err := s.observedPage(rows, removed)
			if err != nil {
				return err
			}
			for _, ev := range page {
				if ev.Yield {
					scope[ev.DeploymentID] = true
					if ev.ForDeployment != 0 {
						scope[ev.ForDeployment] = true
					}
				}
			}
			if len(page) > 0 && r.Scheduling != nil {
				r.Scheduling(page)
			}
			if len(rows) < activityReplayPage {
				break
			}
			after = rows[len(rows)-1].ID + 1
		}
	}
	return nil
}

// observedPage turns a page of scheduling events into what the activity
// counts, the cards of each from where its instances are placed now or sat
// when they were removed.
func (s *Store) observedPage(rows []InstanceEvent, removed map[string]instancePlacement) ([]ObservedEvent, error) {
	named := map[int][]string{}
	var lookup []string
	for i, row := range rows {
		if !IsYield(row) && !IsAutoWake(row) {
			continue
		}
		var params map[string]any
		_ = json.Unmarshal([]byte(row.Data), &params) // historical rows hold free text
		instances := EventInstances(row.InstanceUUID, params)
		named[i] = instances
		lookup = append(lookup, instances...)
	}
	placed, err := s.instancePlacements(lookup)
	if err != nil {
		return nil, err
	}
	for uuid, p := range removed {
		if _, live := placed[uuid]; !live {
			placed[uuid] = p
		}
	}
	out := make([]ObservedEvent, 0, len(rows))
	for i, row := range rows {
		if ev, ok := observedEvent(row, eventCards(named[i], placed)); ok {
			out = append(out, ev)
		}
	}
	return out, nil
}

// replayResidency hands over the residency of a group of deployments.
func (s *Store) replayResidency(r ActivityReplay, ids []uint, replayed map[string]bool) error {
	insts, events, err := s.ResidencyInputsOf(ids, r.Since)
	if err != nil {
		return err
	}
	var out []ObservedEvent
	for _, inst := range insts {
		if inst.EventsFrom != "" || replayed[inst.UUID] {
			continue // a follower sleeps and wakes with its leader
		}
		replayed[inst.UUID] = true
		evs := events[inst.UUID]
		if len(evs) == 0 {
			if inst.Live {
				at := r.Since
				if inst.Start.After(at) {
					at = inst.Start
				}
				out = append(out, seedEvents(inst.UUID, inst.DeploymentID, at, inst.State, inst.Phase, inst.SleepLevel)...)
			}
			continue
		}
		for i := range evs {
			if evs[i].ID > r.ThroughID {
				continue // the observer hands it over
			}
			ev := evs[i]
			at := ev.At
			if at.Before(r.Since) {
				at = r.Since
			}
			out = append(out, ObservedEvent{ID: ev.ID, At: at, DeploymentID: inst.DeploymentID, InstanceUUID: inst.UUID, Residency: &ev})
		}
	}
	sortObserved(out)
	if len(out) > 0 && r.Residency != nil {
		r.Residency(out)
	}
	return nil
}

// replayLiveRows gives every instance not replayed its row's state, dated
// now.
func (s *Store) replayLiveRows(r ActivityReplay, replayed map[string]bool, now time.Time) error {
	var rows []Instance
	if err := s.reader().Select("uuid", "deployment_id", "state", "phase", "sleep_level").Find(&rows).Error; err != nil {
		return err
	}
	var out []ObservedEvent
	for _, row := range rows {
		if !replayed[row.UUID] {
			out = append(out, seedEvents(row.UUID, row.DeploymentID, now, row.State, row.Phase, row.SleepLevel)...)
		}
	}
	if len(out) > 0 && r.Residency != nil {
		r.Residency(out)
	}
	return nil
}

// seedEvents are the events that leave an instance's machine in the state
// its row holds: alive in its state and phase, at its sleep level.
func seedEvents(uuid string, dep uint, at time.Time, state, phase string, level int) []ObservedEvent {
	status := ResidencyEvent{Code: instanceEventStatusUpdated, At: at, State: state, Phase: phase}
	sleep := ResidencyEvent{Code: instanceEventSleepResult, At: at, Result: "applied", Level: level}
	return []ObservedEvent{
		{At: at, DeploymentID: dep, InstanceUUID: uuid, Residency: &status},
		{At: at, DeploymentID: dep, InstanceUUID: uuid, Residency: &sleep},
	}
}

// sortObserved puts events in the order they happened: by time, then by id
// (a seed, id 0, before the events of its own time).
func sortObserved(evs []ObservedEvent) {
	sort.SliceStable(evs, func(i, j int) bool {
		if !evs[i].At.Equal(evs[j].At) {
			return evs[i].At.Before(evs[j].At)
		}
		return evs[i].ID < evs[j].ID
	})
}

// activityReplayQuery reads a page of one kind's scheduling events from id
// after on: a range of the (kind, id) index.
func activityReplayQuery(db *gorm.DB, kind string, codes []string, after, through uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).Where("kind = ? AND id >= ? AND id <= ? AND code IN ?", kind, after, through, codes).
		Order("id").Limit(activityReplayPage)
}
