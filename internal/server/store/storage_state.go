package store

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// StorageHeartbeatInterval is how often the picked node refreshes this control
// plane's heartbeat on a shared storage; StorageHeartbeatFresh is how old a
// heartbeat may be and still count as a control plane in use. The window only
// decides how long a stopped gpuworks still shows as in use; a new one is seen
// at once (it writes on mount). It must cover the refresh interval plus the
// agent's 30 s check loop plus the NFS client's attribute cache (60 s by
// default), about 2.5 minutes. One small write per storage per minute is the
// whole cost, whatever the node count.
const (
	StorageHeartbeatInterval = time.Minute
	StorageHeartbeatFresh    = 5 * time.Minute
)

// ApplyStorageStatus records a node's full storage report. Only rows whose
// reported values changed are written, so a steady fleet writes nothing; a
// storage missing from the report has its row removed. A shared storage's
// capacity is updated when it moves by more than 1%.
func (s *Store) ApplyStorageStatus(nodeID uint, report proto.StorageStatusReport) error {
	var changed []uint
	var transitions []storageTransition
	err := s.db.Transaction(func(tx *gorm.DB) error {
		transitions = nil
		var current []StorageNodeState
		if err := tx.Where("node_id = ?", nodeID).Find(&current).Error; err != nil {
			return err
		}
		byID := make(map[uint]StorageNodeState, len(current))
		for _, row := range current {
			byID[row.StorageID] = row
		}
		reported := map[uint]bool{}
		for _, st := range report.Storages {
			if st.ID == 0 {
				continue
			}
			reported[st.ID] = true
			var storage Storage
			if err := tx.First(&storage, st.ID).Error; err != nil {
				continue // a storage removed while the report was in flight
			}
			next := StorageNodeState{
				StorageID: st.ID, NodeID: nodeID, State: st.State, Reason: truncate(st.Reason, 32),
				Message: truncate(st.Message, 512), Path: truncate(st.Path, 512),
				CachePath: truncate(st.CachePath, 512), Writable: st.Writable,
			}
			prev, ok := byID[st.ID]
			var prevState *StorageNodeState
			if ok {
				prevState = &prev
			}
			if proto.StorageShared(storage.Type) && readyChanged(prevState, next) {
				transitions = append(transitions, storageTransition{storage: storage, ready: next.State == proto.StorageStateReady, state: next})
			}
			if !ok {
				if err := tx.Create(&next).Error; err != nil {
					return err
				}
				changed = append(changed, st.ID)
			} else if prev.State != next.State || prev.Reason != next.Reason || prev.Message != next.Message ||
				prev.Path != next.Path || prev.CachePath != next.CachePath || prev.Writable != next.Writable {
				if err := tx.Model(&StorageNodeState{}).Where("id = ?", prev.ID).Updates(map[string]any{
					"state": next.State, "reason": next.Reason, "message": next.Message, "path": next.Path,
					"cache_path": next.CachePath, "writable": next.Writable, "updated_at": time.Now().UTC(),
				}).Error; err != nil {
					return err
				}
				changed = append(changed, st.ID)
			}
			if proto.StorageShared(storage.Type) && st.State == proto.StorageStateReady && st.TotalBytes > 0 &&
				capacityMoved(storage.TotalBytes, st.TotalBytes, storage.AvailableBytes, st.AvailableBytes) {
				if err := tx.Model(&Storage{}).Where("id = ?", storage.ID).UpdateColumns(map[string]any{
					"total_bytes": st.TotalBytes, "available_bytes": st.AvailableBytes,
				}).Error; err != nil {
					return err
				}
			}
		}
		for id, row := range byID {
			if reported[id] {
				continue
			}
			if err := tx.Delete(&StorageNodeState{}, row.ID).Error; err != nil {
				return err
			}
			changed = append(changed, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.recordStorageTransitions(nodeID, transitions)
	for _, id := range sortedUnique(changed) {
		s.publishStorage(id, "node_state")
	}
	return nil
}

// RecordLegacyModelsRoot keeps the default local storage's node state for an
// agent that predates storages: such an agent reports only its models root in
// the node status.
func (s *Store) RecordLegacyModelsRoot(nodeID uint, root string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	var storage Storage
	err := s.db.First(&storage, s.DefaultLocalStorageID()).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var row StorageNodeState
	err = s.db.Where("storage_id = ? AND node_id = ?", storage.ID, nodeID).First(&row).Error
	if err == nil && row.Path == root && row.State == proto.StorageStateReady {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		row = StorageNodeState{StorageID: storage.ID, NodeID: nodeID, State: proto.StorageStateReady, Path: root, Writable: true}
		if err := s.db.Create(&row).Error; err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if err := s.db.Model(&row).Updates(map[string]any{"state": proto.StorageStateReady, "path": root, "writable": true, "message": "", "reason": ""}).Error; err != nil {
		return err
	}
	s.publishStorage(storage.ID, "node_state")
	return nil
}

func capacityMoved(prevTotal, nextTotal, prevAvail, nextAvail int64) bool {
	if prevTotal <= 0 {
		return true
	}
	diff := func(a, b int64) int64 {
		if a > b {
			return a - b
		}
		return b - a
	}
	return diff(prevTotal, nextTotal)*100 > prevTotal || diff(prevAvail, nextAvail)*100 > prevTotal
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// StorageNodeStates lists every node's state for a storage.
func (s *Store) StorageNodeStates(storageID uint) ([]StorageNodeState, error) {
	var rows []StorageNodeState
	err := s.db.Where("storage_id = ?", storageID).Order("node_id asc").Find(&rows).Error
	return rows, err
}

// StorageNodeStatesForNode lists a node's state for every storage.
func (s *Store) StorageNodeStatesForNode(nodeID uint) ([]StorageNodeState, error) {
	var rows []StorageNodeState
	err := s.db.Where("node_id = ?", nodeID).Order("storage_id asc").Find(&rows).Error
	return rows, err
}

// StorageNodeStateFor loads one node's state for one storage.
func (s *Store) StorageNodeStateFor(storageID, nodeID uint) (*StorageNodeState, error) {
	var row StorageNodeState
	err := s.db.Where("storage_id = ? AND node_id = ?", storageID, nodeID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ReadyStorageNodes lists the nodes on which a storage is ready, oldest node
// first, optionally only those that can write to it.
func (s *Store) ReadyStorageNodes(storageID uint, writable bool) ([]uint, error) {
	q := s.db.Model(&StorageNodeState{}).Where("storage_id = ? AND state = ?", storageID, proto.StorageStateReady)
	if writable {
		q = q.Where("writable = ?", true)
	}
	var ids []uint
	err := q.Order("node_id asc").Pluck("node_id", &ids).Error
	if err != nil || len(ids) == 0 {
		return ids, err
	}
	storage, err := s.StorageByID(storageID)
	if err != nil {
		return nil, err
	}
	targets, err := s.StorageNodes(storageID)
	if err != nil {
		return nil, err
	}
	var nodes []Node
	if err := s.reader().Where("id IN ?", ids).Order("id asc").Find(&nodes).Error; err != nil {
		return nil, err
	}
	ids = ids[:0]
	for i := range nodes {
		if StorageAppliesTo(storage, targets, &nodes[i]) {
			ids = append(ids, nodes[i].ID)
		}
	}
	return ids, nil
}

// HeartbeatNode picks the node that keeps this control plane's heartbeat on a
// shared storage fresh: the lowest node id on which the storage is ready and
// writable and whose agent is connected. The choice is derived, never stored,
// so a node going away hands the duty to the next one.
func (s *Store) HeartbeatNode(storage *Storage) (uint, error) {
	if storage == nil || !proto.StorageShared(storage.Type) {
		return 0, nil
	}
	ids, err := s.ReadyStorageNodes(storage.ID, true)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	var node Node
	err = s.reader().Where("id IN ? AND state = ?", ids, NodeStateOnline).Order("id asc").First(&node).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return node.ID, err
}

// StorageAcceptsWrites reports whether model files may be written into or
// deleted from a storage: it is enabled, and a shared storage has at least one
// node that reports it writable.
func (s *Store) StorageAcceptsWrites(storage *Storage) error {
	if storage == nil {
		return ErrNotFound
	}
	writers := 0
	if storage.Enabled && proto.StorageShared(storage.Type) {
		ids, err := s.ReadyStorageNodes(storage.ID, true)
		if err != nil {
			return err
		}
		writers = len(ids)
	}
	return StorageAcceptsWritesWith(storage, writers)
}

// StorageAcceptsWritesWith is the rule StorageAcceptsWrites applies, given
// how many nodes can write the storage now: a disabled storage takes nothing,
// a local one always can, a shared one needs a writer. A caller that counted
// writers for many storages at once (StorageReadyCounts) asks it directly.
func StorageAcceptsWritesWith(storage *Storage, writers int) error {
	if storage == nil {
		return ErrNotFound
	}
	if !storage.Enabled {
		return ErrStorageDisabled
	}
	if proto.StorageShared(storage.Type) && writers == 0 {
		return ErrStorageNotWritable
	}
	return nil
}

// StorageReadyCount is how many nodes a storage is ready on, and on how many
// of them it is writable.
type StorageReadyCount struct {
	Ready    int
	Writable int
}

// StorageReadyCounts counts currently applicable ready nodes in a fixed number
// of reads. Historical reports from nodes outside the scope do not grant writes.
func (s *Store) StorageReadyCounts() (map[uint]StorageReadyCount, error) {
	var states []StorageNodeState
	if err := s.reader().Where("state = ?", proto.StorageStateReady).Find(&states).Error; err != nil {
		return nil, err
	}
	out := map[uint]StorageReadyCount{}
	if len(states) == 0 {
		return out, nil
	}
	storages, err := s.ListStorages()
	if err != nil {
		return nil, err
	}
	var targets []StorageNode
	if err := s.reader().Find(&targets).Error; err != nil {
		return nil, err
	}
	var nodes []Node
	if err := s.reader().Select("id", "labels").Find(&nodes).Error; err != nil {
		return nil, err
	}
	byStorage := map[uint]*Storage{}
	for i := range storages {
		byStorage[storages[i].ID] = &storages[i]
	}
	byTarget := map[uint][]StorageNode{}
	for _, target := range targets {
		byTarget[target.StorageID] = append(byTarget[target.StorageID], target)
	}
	byNode := map[uint]*Node{}
	for i := range nodes {
		byNode[nodes[i].ID] = &nodes[i]
	}
	for _, state := range states {
		storage, node := byStorage[state.StorageID], byNode[state.NodeID]
		if storage == nil || node == nil || !StorageAppliesTo(storage, byTarget[state.StorageID], node) {
			continue
		}
		count := out[state.StorageID]
		count.Ready++
		if state.Writable {
			count.Writable++
		}
		out[state.StorageID] = count
	}
	return out, nil
}

// ErrStorageNotWritable is a shared storage no node can write to.
var ErrStorageNotWritable = errors.New("no node can write to this storage")
