package store

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// residencyEventChunk bounds the instance UUIDs in one IN list or JSON list.
const residencyEventChunk = 200

var (
	// residencyStateCodes say whether an instance exists and where it is in
	// its start-up; the sleep result says how deeply its engine sleeps.
	residencyStateCodes = []string{instanceEventCreated, instanceEventStateChanged, instanceEventStatusUpdated, EventInstanceRemoved}
	residencyWindowCode = append(append([]string(nil), residencyStateCodes...), instanceEventSleepResult)
)

// residencyEventData is the part of an event's Data the timeline reads.
type residencyEventData struct {
	State  string `json:"state"`
	Phase  string `json:"phase"`
	Level  int    `json:"level"`
	Result string `json:"result"`
	NodeID uint   `json:"node_id"`
	GPUs   []int  `json:"gpus"`
}

func decodeResidencyEvent(row InstanceEvent) ResidencyEvent {
	var data residencyEventData
	_ = json.Unmarshal([]byte(row.Data), &data) // historical rows hold free text
	return ResidencyEvent{
		ID: row.ID, Code: row.Code, At: row.CreatedAt.UTC(),
		State: data.State, Phase: data.Phase, Level: data.Level, Result: data.Result, NodeID: data.NodeID, GPUs: data.GPUs,
	}
}

// ResidencyWindowStart is the id of the first event at or after since, and
// whether there is one. Event ids grow with time, so it turns a time window
// into an id range of the indexes that have the id as their last column.
func (s *Store) ResidencyWindowStart(since time.Time) (uint, bool, error) {
	var rows []InstanceEvent
	if err := residencyBoundaryQuery(s.reader(), since).Find(&rows).Error; err != nil {
		return 0, false, err
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	return rows[0].ID, true, nil
}

// residencyBoundaryQuery reads the first event at or after since by the
// created_at index. Normalize the bound to match UTC timestamps stored as text.
func residencyBoundaryQuery(db *gorm.DB, since time.Time) *gorm.DB {
	return db.Model(&InstanceEvent{}).Select("id").Where("created_at >= ?", since.UTC()).Order("created_at, id").Limit(1)
}

// residencyRemovedQuery reads the INSTANCE_REMOVED events from firstID on, of
// every node: a range of the (kind, id) index. Which node an event names is in
// its data, which is decoded rather than queried (ADR 0032); the range holds
// the removals of the window and nothing else, so it stays small.
func residencyRemovedQuery(db *gorm.DB, firstID uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).Where("kind = ? AND id >= ?", EventKindResidency, firstID)
}

// residencyWindowQuery reads the state and sleep events of some instances
// from firstID on: a range of the (instance_uuid, id) index per instance.
func residencyWindowQuery(db *gorm.DB, uuids []string, firstID uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).Where("instance_uuid IN ? AND id >= ? AND code IN ?", uuids, firstID, residencyWindowCode)
}

// residencyPriorQuery reads the newest events before beforeID among the given
// codes of each instance of uuids, in one statement: each instance's own
// correlated subquery walks the (instance_uuid, id) index backwards. limit is
// how many per instance: a sleep result may be a rolled-back order, which says
// nothing about the level, so the caller reads a few and takes the newest that
// applied. uuids is at most residencyEventChunk long.
func residencyPriorQuery(db *gorm.DB, uuids []string, beforeID uint, codes []string, limit int) *gorm.DB {
	values := strings.TrimSuffix(strings.Repeat("(?),", len(uuids)), ",")
	args := make([]any, 0, len(uuids)+3)
	for _, uuid := range uuids {
		args = append(args, uuid)
	}
	args = append(args, beforeID, codes, limit)
	return db.Raw(`WITH u(uuid) AS (VALUES `+values+`)
		SELECT e.* FROM u JOIN instance_events e ON e.id IN (
		SELECT p.id FROM instance_events p WHERE p.instance_uuid = u.uuid AND p.id < ? AND p.code IN ? ORDER BY p.id DESC LIMIT ?)`, args...)
}

// residencyPriorSleepResults is how many sleep results before the window are
// read to find the newest one that applied.
const residencyPriorSleepResults = 8

// ResidencyInputs gathers what the residency timeline of some nodes over
// [since, now] is derived from: the instances that held their cards then and
// each one's state and sleep events (the ones in the window, and the newest
// before it), per instance in id order. A removed instance is known from its
// INSTANCE_REMOVED event, so removals before the feature wrote it are not
// there.
//
// Every read is a range of an index on its first columns; none sorts or scans
// the timeline: the reads go by node (instances), by kind and id (removals) or
// by instance and id (events).
func (s *Store) ResidencyInputs(nodeIDs []uint, since time.Time) ([]ResidencyInstance, map[string][]ResidencyEvent, error) {
	if len(nodeIDs) == 0 {
		return nil, nil, nil
	}
	onPage := map[uint]bool{}
	for _, id := range nodeIDs {
		onPage[id] = true
	}
	return s.residencyInputs(since, "node_id IN ?", nodeIDs, func(row InstanceEvent, ev ResidencyEvent) bool { return onPage[ev.NodeID] })
}

// ResidencyInputsOf is ResidencyInputs for the instances of some
// deployments, wherever they run: the inputs of the stats of the deployments
// that share GPUs, without those of every instance beside them.
func (s *Store) ResidencyInputsOf(deploymentIDs []uint, since time.Time) ([]ResidencyInstance, map[string][]ResidencyEvent, error) {
	if len(deploymentIDs) == 0 {
		return nil, nil, nil
	}
	wanted := map[uint]bool{}
	for _, id := range deploymentIDs {
		wanted[id] = true
	}
	return s.residencyInputs(since, "deployment_id IN ?", deploymentIDs, func(row InstanceEvent, ev ResidencyEvent) bool { return wanted[row.DeploymentID] })
}

// residencyInputs is the read behind ResidencyInputs and ResidencyInputsOf:
// the live instances matching where (an index on its column, with ids as its
// argument) and the removals that keep says belong to the scope.
func (s *Store) residencyInputs(since time.Time, where string, ids []uint, keep func(InstanceEvent, ResidencyEvent) bool) ([]ResidencyInstance, map[string][]ResidencyEvent, error) {
	db := s.reader()
	firstID, haveWindow, err := s.ResidencyWindowStart(since)
	if err != nil {
		return nil, nil, err
	}
	if !haveWindow {
		firstID = math.MaxInt32 // nothing happened in the window: every event is "before" it
	}
	var rows []Instance
	if err := db.Select("ID", "UUID", "DeploymentID", "NodeID", "State", "Phase", "SleepLevel", "GPUIndexes", "CreatedAt", "Headless", "DistGroup", "DistAttempt").
		Where(where, ids).Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	insts := make([]ResidencyInstance, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		gpus := instanceGPUIndexes(row)
		if len(gpus) == 0 {
			continue
		}
		insts = append(insts, ResidencyInstance{
			UUID: row.UUID, DeploymentID: row.DeploymentID, NodeID: row.NodeID, GPUs: gpus, Live: true, Start: row.CreatedAt.UTC(),
			State: row.State, Phase: row.Phase, SleepLevel: row.SleepLevel,
		})
	}
	if err := s.residencyFollowers(rows, insts); err != nil {
		return nil, nil, err
	}
	if haveWindow {
		var removed []InstanceEvent
		if err := residencyRemovedQuery(db, firstID).Find(&removed).Error; err != nil {
			return nil, nil, err
		}
		live := map[string]bool{}
		for _, inst := range insts {
			live[inst.UUID] = true
		}
		for _, row := range removed {
			ev := decodeResidencyEvent(row)
			if live[row.InstanceUUID] || !keep(row, ev) || len(ev.GPUs) == 0 {
				continue
			}
			insts = append(insts, ResidencyInstance{UUID: row.InstanceUUID, DeploymentID: row.DeploymentID, NodeID: ev.NodeID, GPUs: ev.GPUs})
		}
	}
	sources := map[string]bool{}
	for _, inst := range insts {
		sources[inst.EventsOf()] = true
	}
	events, err := s.residencyEvents(sources, firstID, haveWindow)
	return insts, events, err
}

// EventsOf is the instance whose events drive this one.
func (r ResidencyInstance) EventsOf() string {
	if r.EventsFrom != "" {
		return r.EventsFrom
	}
	return r.UUID
}

// residencyFollowers points each live headless follower at its leader: a
// multi-node replica sleeps and wakes as one engine, which only the leader
// reports.
func (s *Store) residencyFollowers(rows []Instance, insts []ResidencyInstance) error {
	deployments := map[uint]bool{}
	for i := range rows {
		if rows[i].Headless && rows[i].DistGroup != "" {
			deployments[rows[i].DeploymentID] = true
		}
	}
	if len(deployments) == 0 {
		return nil
	}
	ids := make([]uint, 0, len(deployments))
	for id := range deployments {
		ids = append(ids, id)
	}
	var leaders []Instance
	if err := s.reader().Select("uuid", "deployment_id", "dist_group", "dist_attempt").
		Where("deployment_id IN ? AND headless = ? AND dist_group <> ''", ids, false).Find(&leaders).Error; err != nil {
		return err
	}
	type group struct {
		deployment uint
		name       string
		attempt    int
	}
	leaderOf := map[group]string{}
	for _, l := range leaders {
		leaderOf[group{l.DeploymentID, l.DistGroup, l.DistAttempt}] = l.UUID
	}
	byUUID := map[string]*Instance{}
	for i := range rows {
		byUUID[rows[i].UUID] = &rows[i]
	}
	for i := range insts {
		row := byUUID[insts[i].UUID]
		if row == nil || !row.Headless || row.DistGroup == "" {
			continue
		}
		insts[i].EventsFrom = leaderOf[group{row.DeploymentID, row.DistGroup, row.DistAttempt}]
	}
	return nil
}

// residencyEvents reads each source instance's events: the newest before the
// window (state and sleep level), then those inside it, in id order.
func (s *Store) residencyEvents(sources map[string]bool, firstID uint, haveWindow bool) (map[string][]ResidencyEvent, error) {
	db := s.reader()
	out := map[string][]ResidencyEvent{}
	uuids := make([]string, 0, len(sources))
	for uuid := range sources {
		if uuid != "" {
			uuids = append(uuids, uuid)
		}
	}
	for start := 0; start < len(uuids); start += residencyEventChunk {
		chunk := uuids[start:min(start+residencyEventChunk, len(uuids))]
		var state, sleeps []InstanceEvent
		if err := residencyPriorQuery(db, chunk, firstID, residencyStateCodes, 1).Find(&state).Error; err != nil {
			return nil, err
		}
		if err := residencyPriorQuery(db, chunk, firstID, []string{instanceEventSleepResult}, residencyPriorSleepResults).Find(&sleeps).Error; err != nil {
			return nil, err
		}
		for _, row := range state {
			out[row.InstanceUUID] = append(out[row.InstanceUUID], decodeResidencyEvent(row))
		}
		// Newest first, so each instance keeps its newest sleep result that applied.
		sort.Slice(sleeps, func(i, j int) bool { return sleeps[i].ID > sleeps[j].ID })
		took := map[string]bool{}
		for _, row := range sleeps {
			if took[row.InstanceUUID] {
				continue
			}
			if ev := decodeResidencyEvent(row); ev.Result == "applied" {
				out[row.InstanceUUID] = append(out[row.InstanceUUID], ev)
				took[row.InstanceUUID] = true
			}
		}
	}
	if haveWindow {
		for start := 0; start < len(uuids); start += residencyEventChunk {
			end := min(start+residencyEventChunk, len(uuids))
			var rows []InstanceEvent
			if err := residencyWindowQuery(db, uuids[start:end], firstID).Find(&rows).Error; err != nil {
				return nil, err
			}
			for _, row := range rows {
				out[row.InstanceUUID] = append(out[row.InstanceUUID], decodeResidencyEvent(row))
			}
		}
	}
	for uuid := range out {
		SortResidencyEvents(out[uuid])
	}
	return out, nil
}
