package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// ErrNetworkTestRunning reports a second test of a network while one runs.
var ErrNetworkTestRunning = errors.New("a test of this network is already running")

// networkTestKeep is how many finished runs a network keeps.
const networkTestKeep = 10

// networkTestStale is when a still-running row can only be a run the server
// lost by restarting; no run lives that long (listener TTLs cap at an hour).
const networkTestStale = 2 * time.Hour

// StartNetworkTestRun records a new running test, refusing while another
// test of the same network is running.
func (s *Store) StartNetworkTestRun(run *NetworkTestRun) (*NetworkTestRun, error) {
	now := time.Now().UTC()
	err := s.db.Transaction(func(tx *gorm.DB) error {
		// Network 0 is the management network, which has no row.
		if run.NetworkID != 0 {
			if err := tx.First(&Network{}, run.NetworkID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
		}
		if err := tx.Model(&NetworkTestRun{}).
			Where("network_id = ? AND status = ? AND started_at < ?", run.NetworkID, NetworkTestRunning, now.Add(-networkTestStale)).
			Updates(map[string]any{"status": NetworkTestInterrupted, "finished_at": now}).Error; err != nil {
			return err
		}
		var running int64
		if err := tx.Model(&NetworkTestRun{}).Where("network_id = ? AND status = ?", run.NetworkID, NetworkTestRunning).Count(&running).Error; err != nil {
			return err
		}
		if running > 0 {
			return ErrNetworkTestRunning
		}
		run.Status, run.StartedAt, run.FinishedAt = NetworkTestRunning, now, nil
		return tx.Create(run).Error
	})
	if err != nil {
		return nil, err
	}
	s.Bus.PublishChange("network", uintKey(run.NetworkID), "test_started")
	// The controller runs the test; the API only records the request.
	s.Bus.PublishChange("network_test", uintKey(run.ID), "started")
	return run, nil
}

// NetworkTestRunByID loads one run.
func (s *Store) NetworkTestRunByID(id uint) (*NetworkTestRun, error) {
	var run NetworkTestRun
	err := s.db.First(&run, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	return &run, err
}

// InterruptRunningNetworkTests marks runs a previous server process started
// as interrupted; their agent-side listeners expire on their own TTL.
func (s *Store) InterruptRunningNetworkTests() error {
	return s.db.Model(&NetworkTestRun{}).Where("status = ?", NetworkTestRunning).
		Updates(map[string]any{"status": NetworkTestInterrupted, "finished_at": time.Now().UTC()}).Error
}

// AbandonNetworkTestRun marks a run that could not be executed at all.
func (s *Store) AbandonNetworkTestRun(id uint) error {
	var run NetworkTestRun
	if err := s.db.First(&run, id).Error; err != nil {
		return err
	}
	if err := s.db.Model(&run).Updates(map[string]any{"status": NetworkTestInterrupted, "finished_at": time.Now().UTC()}).Error; err != nil {
		return err
	}
	s.Bus.PublishChange("network", uintKey(run.NetworkID), "test_finished")
	return nil
}

// FinishNetworkTestRun stores a run's results and prunes old runs.
func (s *Store) FinishNetworkTestRun(id uint, pairsJSON string, pairCount, passed int) error {
	var run NetworkTestRun
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&run, id).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		if err := tx.Model(&run).Updates(map[string]any{
			"status": NetworkTestDone, "pairs": pairsJSON, "pair_count": pairCount, "passed_count": passed, "finished_at": now,
		}).Error; err != nil {
			return err
		}
		var keep []uint
		if err := tx.Model(&NetworkTestRun{}).Where("network_id = ?", run.NetworkID).Order("id desc").Limit(networkTestKeep).Pluck("id", &keep).Error; err != nil {
			return err
		}
		return tx.Where("network_id = ? AND id NOT IN ?", run.NetworkID, keep).Delete(&NetworkTestRun{}).Error
	})
	if err != nil {
		return err
	}
	s.Bus.PublishChange("network", uintKey(run.NetworkID), "test_finished")
	return nil
}

// LatestNetworkTestRun is the newest run of a network, if any.
func (s *Store) LatestNetworkTestRun(networkID uint) (*NetworkTestRun, error) {
	var run NetworkTestRun
	err := s.db.Where("network_id = ?", networkID).Order("id desc").First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if run.Status == NetworkTestRunning && time.Since(run.StartedAt) > networkTestStale {
		run.Status = NetworkTestInterrupted
	}
	return &run, nil
}
