package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"

	"gorm.io/gorm"
)

type schedulingEpochState struct {
	mu      sync.Mutex
	value   uint64
	set     bool
	lost    bool
	owner   string
	lossSeq uint64
}

var schedulingEpochs sync.Map

func (s *Store) schedulingEpochState() *schedulingEpochState {
	db := s.schedulingWriter()
	if db == nil {
		return &schedulingEpochState{}
	}
	if state, ok := schedulingEpochs.Load(db); ok {
		return state.(*schedulingEpochState)
	}
	state := &schedulingEpochState{}
	actual, _ := schedulingEpochs.LoadOrStore(db, state)
	return actual.(*schedulingEpochState)
}

const schedulingGenerationSetting = "scheduling.generation"
const schedulingOwnerSetting = "scheduling.owner"
const schedulingConfirmationSetting = "scheduling.confirmation"

var ErrStaleSchedulingGeneration = errors.New("scheduling generation changed")

func (s *Store) schedulingWriter() *gorm.DB {
	if s.schedulingDB != nil {
		return s.schedulingDB
	}
	return s.db
}

// SchedulingGeneration is the current scheduling epoch. Failover advances it.
func (s *Store) SchedulingGeneration(ctx context.Context) (uint64, error) {
	var row Setting
	err := s.schedulingWriter().WithContext(ctx).First(&row, "key = ?", schedulingGenerationSetting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseUint(row.Value, 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}

// NoteSchedulingLeadership pins the epoch while this process is the leader and
// advances it when that leadership is lost. Later scheduling transactions are
// fenced against the pinned epoch.
func (s *Store) NoteSchedulingLeadership(ctx context.Context, leader bool) error {
	state := s.schedulingEpochState()
	if !leader {
		// Mark the epoch lost before the database write. An open placement
		// checks this flag and rolls back, which releases the write lock so
		// the generation can advance.
		state.mu.Lock()
		state.set = false
		state.lost = true
		state.owner = ""
		state.lossSeq++
		state.mu.Unlock()
		if _, err := s.AdvanceSchedulingGeneration(ctx); err != nil {
			return err
		}
		return nil
	}
	current, err := s.SchedulingGeneration(ctx)
	if err != nil {
		return err
	}
	state.mu.Lock()
	state.value = current
	state.set = true
	state.lost = false
	state.mu.Unlock()
	return nil
}

// BeginSchedulingLeadership commits one new generation for this process's
// acquisition before its controller starts. A lost commit acknowledgement may
// be retried with the same owner token; the retry confirms quorum with another
// distinct write but does not create another generation. A different process
// or an observed loss uses a new token and therefore advances the generation.
func (s *Store) BeginSchedulingLeadership(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, schedulingTransactionTimeout)
	defer cancel()
	local, err := s.LocalSchedulingLeader(ctx)
	if err != nil {
		return err
	}
	if !local {
		return ErrStaleSchedulingGeneration
	}
	state := s.schedulingEpochState()
	state.mu.Lock()
	owner := state.owner
	lossSeq := state.lossSeq
	if owner == "" {
		owner, err = newSchedulingToken()
		if err == nil {
			state.owner = owner
		}
	}
	state.mu.Unlock()
	if err != nil {
		return err
	}
	confirmation, err := newSchedulingToken()
	if err != nil {
		return err
	}
	var generation uint64
	err = s.schedulingWriteTransaction(ctx, func(tx *gorm.DB) error {
		var row Setting
		if err := tx.First(&row, "key = ?", schedulingOwnerSetting).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if row.Value != owner {
			current, err := readSchedulingGeneration(tx)
			if err != nil {
				return err
			}
			if current == ^uint64(0) {
				return errors.New("scheduling generation exhausted")
			}
			generation = current + 1
			if err := tx.Save(&Setting{Key: schedulingGenerationSetting, Value: strconv.FormatUint(generation, 10)}).Error; err != nil {
				return err
			}
			if err := tx.Save(&Setting{Key: schedulingOwnerSetting, Value: owner}).Error; err != nil {
				return err
			}
		} else {
			var err error
			generation, err = readSchedulingGeneration(tx)
			if err != nil {
				return err
			}
			if generation == 0 {
				return errors.New("scheduling owner has no generation")
			}
		}
		return tx.Save(&Setting{Key: schedulingConfirmationSetting, Value: confirmation}).Error
	})
	if err != nil {
		return err
	}
	local, err = s.LocalSchedulingLeader(ctx)
	if err != nil {
		return err
	}
	if !local {
		return ErrStaleSchedulingGeneration
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.owner != owner || state.lossSeq != lossSeq {
		return ErrStaleSchedulingGeneration
	}
	state.value = generation
	state.set = true
	state.lost = false
	return nil
}

func newSchedulingToken() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func (s *Store) observedSchedulingGeneration(ctx context.Context) (uint64, error) {
	state := s.schedulingEpochState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.lost {
		return state.value, nil
	}
	if state.set {
		return state.value, nil
	}
	current, err := s.SchedulingGeneration(ctx)
	if err != nil {
		return 0, err
	}
	state.value = current
	state.set = true
	return current, nil
}

// AdvanceSchedulingGeneration starts a new epoch on the scheduling connection.
// It does not write the ordinary pool. The previous epoch stays pinned, so a
// placement that began before this call cannot commit.
func (s *Store) AdvanceSchedulingGeneration(ctx context.Context) (uint64, error) {
	var next uint64
	err := s.schedulingWriteTransaction(ctx, func(tx *gorm.DB) error {
		current, err := readSchedulingGeneration(tx)
		if err != nil {
			return err
		}
		next = current + 1
		return tx.Save(&Setting{Key: schedulingGenerationSetting, Value: strconv.FormatUint(next, 10)}).Error
	})
	if err != nil {
		return 0, err
	}
	state := s.schedulingEpochState()
	state.mu.Lock()
	state.lost = true
	state.set = false
	state.owner = ""
	state.lossSeq++
	state.mu.Unlock()
	return next, nil
}

// RejectStaleSchedulingGeneration reports whether this process may still commit
// a scheduling decision. Leadership loss refuses before a controller pushes work.
func (s *Store) RejectStaleSchedulingGeneration(ctx context.Context) error {
	epoch, err := s.observedSchedulingGeneration(ctx)
	if err != nil {
		return err
	}
	return s.rejectStaleSchedulingEpoch(s.schedulingWriter().WithContext(ctx), epoch)
}

// commitSchedulingDecision fences a scheduling mutation that does not go through
// WithSchedulingTx. The generation is checked on the scheduling connection
// before and after the write. Leadership loss rolls the mutation back.
func (s *Store) commitSchedulingDecision(ctx context.Context, fn func(tx *gorm.DB) error) error {
	if s.fencedSchedulingTx {
		return fn(s.db)
	}
	if s.agentStatusTx {
		return s.db.Transaction(fn)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, endWork := s.bindMaintenanceWork(ctx, MaintenanceWorkTransaction)
	defer endWork()
	if err := s.rejectKnownRemoteSchedulingLeader(ctx); err != nil {
		return err
	}
	epoch, err := s.observedSchedulingGeneration(ctx)
	if err != nil {
		return err
	}
	return s.schedulingWriteTransaction(ctx, func(tx *gorm.DB) error {
		if err := s.rejectStaleSchedulingEpoch(tx, epoch); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return s.rejectStaleSchedulingEpoch(tx, epoch)
	})
}

// rejectStaleSchedulingEpoch fails a placement whose epoch is no longer current.
// Leadership loss is recorded in memory before the generation row changes, so
// an open transaction observes it without reading a snapshot of the old row.
func (s *Store) rejectStaleSchedulingEpoch(tx *gorm.DB, epoch uint64) error {
	state := s.schedulingEpochState()
	state.mu.Lock()
	lost := state.lost
	state.mu.Unlock()
	if lost {
		return ErrStaleSchedulingGeneration
	}
	got, err := readSchedulingGeneration(tx)
	if err != nil {
		return err
	}
	if got != epoch {
		return ErrStaleSchedulingGeneration
	}
	return nil
}

// RequireGeneration rejects the surrounding scheduling transaction when the
// epoch changed after the decision was read.
func (t *SchedulingTx) RequireGeneration(want uint64) error {
	got, err := readSchedulingGeneration(t.store.db)
	if err != nil {
		t.check(err)
		return err
	}
	if got != want {
		t.err = ErrStaleSchedulingGeneration
		return t.err
	}
	return nil
}

func readSchedulingGeneration(db *gorm.DB) (uint64, error) {
	var row Setting
	err := db.First(&row, "key = ?", schedulingGenerationSetting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(row.Value, 10, 64)
}
