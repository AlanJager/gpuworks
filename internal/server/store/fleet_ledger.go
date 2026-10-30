package store

import (
	"encoding/json"
	"slices"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// The reads below serve the scheduling center's overview of the whole fleet
// (planning.FleetCards): the ledger of every node from one read of the live
// instances rather than one read per node.

// liveInstanceStates are the states an instance can charge a card in: every
// state but stopped. They are listed (not "<> stopped") so that the read is
// a set of ranges of the state index.
func liveInstanceStates() []string {
	states := make([]string, 0, 16)
	for _, state := range proto.InstanceStates() {
		if state != proto.InstStopped {
			states = append(states, state)
		}
	}
	return states
}

func liveInstancesQuery(db *gorm.DB) *gorm.DB {
	return db.Model(&Instance{}).Where("state IN ?", liveInstanceStates())
}

// LiveInstances are the instances of every node that are not stopped, the
// rows the allocation ledger is drawn from.
func (s *Store) LiveInstances() ([]Instance, error) {
	var out []Instance
	err := liveInstancesQuery(s.reader()).Find(&out).Error
	return out, err
}

func ledgerDeploymentsQuery(db *gorm.DB) *gorm.DB {
	return db.Model(&Deployment{}).Select(append([]string{"id", "name", "model_id"}, yieldPolicyColumns...))
}

// LedgerDeployments are every deployment with only what the scheduling
// overview names it by and the ledger sizes a sleeper from: id, name, yield
// policy and model.
func (s *Store) LedgerDeployments() ([]Deployment, error) {
	var out []Deployment
	err := ledgerDeploymentsQuery(s.reader()).Find(&out).Error
	return out, err
}

// ledgerModelChunk bounds the ids of one IN list.
const ledgerModelChunk = 500

// LedgerModels are the models with the given ids, with only what sizes a
// sleeper's residue: the weights and the format they are stored in.
func (s *Store) LedgerModels(ids []uint) ([]Model, error) {
	var out []Model
	for start := 0; start < len(ids); start += ledgerModelChunk {
		chunk := ids[start:min(start+ledgerModelChunk, len(ids))]
		var rows []Model
		if err := s.reader().Select("id", "weight_bytes", "quant_label", "dtype").Where("id IN ?", chunk).Find(&rows).Error; err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// yieldEventsQuery reads the automation events from firstID on that may
// record a deployment giving its cards up for another (IsYield tells which
// do): a range of the (kind, id) index that holds the automation events of
// the window and nothing older.
func yieldEventsQuery(db *gorm.DB, firstID uint) *gorm.DB {
	return db.Model(&InstanceEvent{}).
		Where("kind = ? AND id >= ? AND code IN ?", EventKindAutomation, firstID, []string{codeAutoSleep, codeYieldedForWake})
}

// YieldEvents are the sleeps ordered for another deployment since the time,
// from which the caller counts the yields with IsYield.
func (s *Store) YieldEvents(since time.Time) ([]InstanceEvent, error) {
	first, ok, err := s.ResidencyWindowStart(since)
	if err != nil || !ok {
		return nil, err
	}
	var out []InstanceEvent
	err = yieldEventsQuery(s.reader(), first).Find(&out).Error
	return out, err
}

// SharedDeploymentIDs are the ids of the deployments that share GPUs: those
// of a yield policy other than never, and any whose live instances sit
// on a card together with another deployment's, sorted.
func (s *Store) SharedDeploymentIDs() ([]uint, error) {
	var ids []uint
	if err := s.reader().Model(&Deployment{}).Where("yield_policy NOT IN ?", []string{"", yieldpolicy.Never}).Pluck("id", &ids).Error; err != nil {
		return nil, err
	}
	// Deployments whose live instances sit on one card together share it
	// whatever their level (a resident one asleep beside another, say).
	live, err := s.LiveInstances()
	if err != nil {
		return nil, err
	}
	type card struct {
		node  uint
		index int
	}
	onCard := map[card]map[uint]bool{}
	for _, in := range live {
		var indexes []int
		if in.GPUIndexes == "" || json.Unmarshal([]byte(in.GPUIndexes), &indexes) != nil {
			continue
		}
		for _, index := range indexes {
			k := card{in.NodeID, index}
			if onCard[k] == nil {
				onCard[k] = map[uint]bool{}
			}
			onCard[k][in.DeploymentID] = true
		}
	}
	seen := map[uint]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, deps := range onCard {
		if len(deps) < 2 {
			continue
		}
		for id := range deps {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	slices.Sort(ids)
	return ids, nil
}
