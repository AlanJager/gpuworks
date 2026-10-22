package app

import (
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// trashSweepAge is how long a removed model may sit in .gpuworks/trash before
// the heartbeat node deletes it: well past any background removal in flight.
const trashSweepAge = 30 * time.Minute

// refreshHeartbeats writes this control plane's heartbeat on every shared
// storage the server picked this node for, at the interval it asked.
func (n *NodeRuntime) refreshHeartbeats() {
	s := n.storage
	s.mu.Lock()
	type due struct {
		id   uint
		root string
		hb   proto.StorageHeartbeat
	}
	var work []due
	for id, task := range s.tasks {
		st, ok := s.statuses[id]
		if task.Heartbeat == nil || !ok || st.State != storagemount.StateReady || !st.Writable {
			continue
		}
		if task.Heartbeat.IntervalSeconds <= 0 {
			// The server always states the interval; guessing one would hide
			// that it does not.
			if !s.hbInvalid[id] {
				s.hbInvalid[id] = true
				n.log.Error("storage heartbeat skipped: the server sent no positive interval", "storage", id,
					"interval_seconds", task.Heartbeat.IntervalSeconds)
			}
			continue
		}
		delete(s.hbInvalid, id)
		if time.Since(s.heartbeat[id]) < time.Duration(task.Heartbeat.IntervalSeconds)*time.Second {
			continue
		}
		s.heartbeat[id] = time.Now()
		work = append(work, due{id: id, root: n.storageSpec(task).Path, hb: *task.Heartbeat})
	}
	s.mu.Unlock()
	for _, item := range work {
		go n.writeHeartbeat(item.id, item.root, item.hb)
	}
}

// writeHeartbeat writes one heartbeat and then sweeps the storage's trash.
// Only the heartbeat node sweeps, so a large cluster adds one directory
// listing per storage per interval.
func (n *NodeRuntime) writeHeartbeat(id uint, root string, hb proto.StorageHeartbeat) {
	err := n.storageDo(id, "heartbeat", n.storageTimeouts().heartbeat, func() error {
		_, err := storagemount.WriteHeartbeat(root, storagemount.Heartbeat{
			ID: hb.ControlPlaneID, Name: hb.ControlPlaneName, Version: hb.Version,
		})
		return err
	})
	if err != nil {
		n.log.Warn("storage heartbeat failed", "storage", id, "err", err)
		return
	}
	err = n.storageDo(id, "trash", n.storageTimeouts().sweep, func() error {
		_, err := storagemount.SweepTrash(root, trashSweepAge, time.Now())
		return err
	})
	if err != nil {
		n.log.Warn("storage trash sweep failed", "storage", id, "err", err)
	}
	n.sweepIncoming(id, root)
}
