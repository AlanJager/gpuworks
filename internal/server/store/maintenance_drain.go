package store

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gorm.io/gorm"
)

const (
	// MaintenanceWorkRPC is an external call that already left this process.
	MaintenanceWorkRPC = "rpc"
	// MaintenanceWorkStream is an open byte stream or tunnel.
	MaintenanceWorkStream = "stream"
	// MaintenanceWorkTransaction is a database transaction that has not committed.
	MaintenanceWorkTransaction = "transaction"
	// MaintenanceWorkBackground is a worker that can still write or issue commands.
	MaintenanceWorkBackground = "background"
)

var (
	// ErrMaintenanceWorkOutstanding means in-flight work has not finished or its outcome is still unknown.
	ErrMaintenanceWorkOutstanding = errors.New("maintenance work is still outstanding")
	// ErrMaintenanceWorkUnknown is reported by an owner that stopped without a known commit or rollback.
	ErrMaintenanceWorkUnknown = errors.New("maintenance work outcome is unknown")
	// ErrMaintenanceMemberNotIsolated means a live member can still write, so this is not a safe point.
	ErrMaintenanceMemberNotIsolated = errors.New("unisolated member blocks the safe maintenance point")
)

type maintenanceWorkItem struct {
	cancel        func()
	cancelStarted bool
	finished      bool
	unknown       bool
	done          chan struct{}
	closeOnce     sync.Once
}

type maintenanceWorkSet struct {
	mu    sync.Mutex
	seq   uint64
	items map[uint64]*maintenanceWorkItem
}

// maintenanceToken keeps the lock off Store. Value copies used inside an open
// transaction share this pointer, so the drain still sees their work.
type maintenanceToken struct {
	set *maintenanceWorkSet
}

var maintenanceTokenInit sync.Mutex

func (s *Store) maintenanceWorks() *maintenanceWorkSet {
	maintenanceTokenInit.Lock()
	defer maintenanceTokenInit.Unlock()
	if s.maintenanceToken == nil {
		s.maintenanceToken = &maintenanceToken{set: &maintenanceWorkSet{items: map[uint64]*maintenanceWorkItem{}}}
	}
	return s.maintenanceToken.set
}

// TrackMaintenanceWork registers one in-flight RPC, stream, transaction, or background job.
// cancel asks it to stop. finish(nil) or finish of any other error records a known outcome.
// finish(ErrMaintenanceWorkUnknown) keeps the safe point closed until a later known finish.
func (s *Store) TrackMaintenanceWork(kind string, cancel func()) (func(error), error) {
	switch kind {
	case MaintenanceWorkRPC, MaintenanceWorkStream, MaintenanceWorkTransaction, MaintenanceWorkBackground:
	default:
		return nil, fmt.Errorf("unknown maintenance work kind %q", kind)
	}
	set := s.maintenanceWorks()
	item := &maintenanceWorkItem{cancel: cancel, done: make(chan struct{})}
	set.mu.Lock()
	set.seq++
	id := set.seq
	set.items[id] = item
	set.mu.Unlock()
	return func(outcome error) {
		set.mu.Lock()
		defer set.mu.Unlock()
		current, ok := set.items[id]
		if !ok {
			return
		}
		if outcome != nil && errors.Is(outcome, ErrMaintenanceWorkUnknown) {
			current.unknown = true
			current.finished = true
			current.closeOnce.Do(func() { close(current.done) })
			return
		}
		delete(set.items, id)
		current.finished = true
		current.unknown = false
		current.closeOnce.Do(func() { close(current.done) })
	}, nil
}

// bindMaintenanceWork tracks one transaction until the caller returns.
// The returned end is safe to defer. Cancelling the context does not by itself record the outcome.
func (s *Store) bindMaintenanceWork(ctx context.Context, kind string) (context.Context, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	finish, err := s.TrackMaintenanceWork(kind, cancel)
	if err != nil {
		cancel()
		return ctx, func() {}
	}
	return ctx, func() {
		finish(nil)
		cancel()
	}
}

// EstablishSafeMaintenancePoint cancels in-flight work and moves frozen to drained
// only after every item has a known outcome and every live member is isolated.
// An unisolated member or an unknown outcome leaves the phase unchanged.
func (s *Store) EstablishSafeMaintenancePoint(ctx context.Context, expected MaintenancePhaseState, isolatedFingerprints []string) (MaintenancePhaseState, error) {
	if err := ctx.Err(); err != nil {
		return MaintenancePhaseState{}, err
	}
	if expected.Phase != MaintenanceFrozen || expected.Revision <= 0 {
		return MaintenancePhaseState{}, ErrMaintenanceTransitionInvalid
	}
	if err := s.cancelAndReconcileMaintenanceWork(ctx); err != nil {
		return MaintenancePhaseState{}, err
	}
	isolated := make(map[string]struct{}, len(isolatedFingerprints))
	for _, fingerprint := range isolatedFingerprints {
		isolated[fingerprint] = struct{}{}
	}
	set := s.maintenanceWorks()
	set.mu.Lock()
	defer set.mu.Unlock()
	if maintenanceWorkOutstanding(set) {
		return MaintenancePhaseState{}, ErrMaintenanceWorkOutstanding
	}
	return s.commitDrainedMaintenancePoint(ctx, expected, isolated)
}

func maintenanceWorkOutstanding(set *maintenanceWorkSet) bool {
	for _, item := range set.items {
		if item.unknown || !item.finished {
			return true
		}
	}
	return false
}

func (s *Store) cancelAndReconcileMaintenanceWork(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("%w: %w", ErrMaintenanceWorkOutstanding, err)
		}
		set := s.maintenanceWorks()
		set.mu.Lock()
		unknown := false
		var cancels []func()
		var waiting []chan struct{}
		for _, item := range set.items {
			if item.unknown {
				unknown = true
				continue
			}
			if item.finished {
				continue
			}
			if item.cancel != nil && !item.cancelStarted {
				item.cancelStarted = true
				cancels = append(cancels, item.cancel)
			}
			waiting = append(waiting, item.done)
		}
		set.mu.Unlock()
		if unknown {
			return ErrMaintenanceWorkOutstanding
		}
		for _, cancel := range cancels {
			cancel()
		}
		if len(waiting) == 0 {
			return nil
		}
		select {
		case <-waiting[0]:
		case <-ctx.Done():
			return fmt.Errorf("%w: %w", ErrMaintenanceWorkOutstanding, ctx.Err())
		}
	}
}

func (s *Store) commitDrainedMaintenancePoint(ctx context.Context, expected MaintenancePhaseState, isolated map[string]struct{}) (MaintenancePhaseState, error) {
	var stale bool
	var blocked bool
	err := s.schedulingWriter().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var phase string
		var revision int64
		if err := tx.Raw(`SELECT phase, revision FROM maintenance_phase WHERE id = 1`).Row().Scan(&phase, &revision); err != nil {
			return err
		}
		if phase != expected.Phase || revision != expected.Revision {
			stale = true
			return ErrMaintenancePhaseChanged
		}
		var identity ClusterIdentity
		err := tx.First(&identity, 1).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil {
			var members []ClusterAdmission
			if err := tx.Where("revoked_at IS NULL").Find(&members).Error; err != nil {
				return err
			}
			for _, member := range members {
				if _, ok := isolated[member.KeyFingerprint]; !ok {
					blocked = true
					return ErrMaintenanceMemberNotIsolated
				}
			}
		}
		result := tx.Exec(`UPDATE maintenance_phase
			SET phase = ?, revision = revision + 1, updated_at = CURRENT_TIMESTAMP
			WHERE id = 1 AND phase = ? AND revision = ?`, MaintenanceDrained, expected.Phase, expected.Revision)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			stale = true
			return ErrMaintenancePhaseChanged
		}
		return nil
	})
	if err != nil {
		if blocked && errors.Is(err, ErrMaintenanceMemberNotIsolated) {
			return MaintenancePhaseState{}, ErrMaintenanceMemberNotIsolated
		}
		if stale && errors.Is(err, ErrMaintenancePhaseChanged) {
			return MaintenancePhaseState{}, ErrMaintenancePhaseChanged
		}
		return MaintenancePhaseState{}, fmt.Errorf("%w: %w", ErrMaintenanceCommitUnconfirmed, err)
	}
	return MaintenancePhaseState{Phase: MaintenanceDrained, Revision: expected.Revision + 1}, nil
}
