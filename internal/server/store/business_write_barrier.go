package store

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"

	"gorm.io/gorm"
)

var ErrBusinessWritesClosed = errors.New("business writes are closed")

// businessWriteBarrier guards ordinary replicated business writes. The
// eligibility callback must read cheap, local cached state: it runs inside a
// database transaction and must not perform network or replicated DB I/O.
type businessWriteBarrier struct {
	mu         sync.Mutex
	open       bool
	active     int
	generation uint64
	idle       chan struct{}
	eligible   func(context.Context) error
	depth      map[int64]int
}

// InstallBusinessWriteBarrier enables the joined-store gate in its closed
// state. Stores without an installed barrier keep the single-member behavior.
// Install before exposing the Store to concurrent business callers.
func (s *Store) InstallBusinessWriteBarrier(eligible func(context.Context) error) error {
	if eligible == nil {
		return errors.New("business write eligibility is required")
	}
	idle := make(chan struct{})
	close(idle)
	b := &businessWriteBarrier{idle: idle, eligible: eligible}
	if s.businessWriteBarrier != nil {
		return errors.New("business write barrier is already installed")
	}
	s.businessWriteBarrier = b
	return nil
}

// OpenBusinessWrites admits new business transactions after the caller has
// established owner eligibility. A close still draining cannot be reopened.
func (s *Store) OpenBusinessWrites() error {
	b := s.businessWriteBarrier
	if b == nil {
		return nil
	}
	b.mu.Lock()
	if b.active != 0 {
		b.mu.Unlock()
		return errors.New("business writes are still draining")
	}
	generation := b.generation
	b.mu.Unlock()
	// Eligibility may itself invalidate the gate. Check it outside the lock,
	// then reject an open that crossed any concurrent suspension.
	if err := b.eligible(context.Background()); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.generation != generation {
		return ErrBusinessWritesClosed
	}
	if b.active != 0 {
		return errors.New("business writes are still draining")
	}
	b.open = true
	return nil
}

// SuspendBusinessWrites synchronously stops new admissions without waiting for
// already admitted transactions. Material invalidation callbacks use this
// before the owner starts a bounded drain outside the callback.
func (s *Store) SuspendBusinessWrites() {
	s.suspendBusinessWrites()
}

func (s *Store) suspendBusinessWrites() <-chan struct{} {
	b := s.businessWriteBarrier
	if b == nil {
		return nil
	}
	b.mu.Lock()
	b.open = false
	b.generation++
	idle := b.idle
	b.mu.Unlock()
	return idle
}

// CloseBusinessWrites stops admissions first and then waits for transactions
// already admitted to finish commit or rollback. A timed-out wait leaves the
// gate closed, and a later call can finish waiting on the same drain.
func (s *Store) CloseBusinessWrites(ctx context.Context) error {
	idle := s.suspendBusinessWrites()
	if idle == nil {
		return nil
	}
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *businessWriteBarrier) acquire() (func(), error) {
	id := currentGID()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return nil, ErrBusinessWritesClosed
	}
	if b.depth == nil {
		b.depth = map[int64]int{}
	}
	b.depth[id]++
	if b.active == 0 {
		b.idle = make(chan struct{})
	}
	b.active++
	return func() {
		b.mu.Lock()
		b.depth[id]--
		if b.depth[id] == 0 {
			delete(b.depth, id)
		}
		b.active--
		if b.active == 0 {
			close(b.idle)
		}
		b.mu.Unlock()
	}, nil
}

// holding reports whether this goroutine already owns an admitted business
// write. A commit callback may close new admissions before that write's own
// publication marker; the marker is still part of the admitted call.
func (b *businessWriteBarrier) holding() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.depth[currentGID()] > 0
}

func currentGID() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	line := buf[:n]
	const prefix = "goroutine "
	line = line[len(prefix):]
	end := 0
	for end < len(line) && line[end] != ' ' {
		end++
	}
	id, _ := strconv.ParseInt(string(line[:end]), 10, 64)
	return id
}

func (b *businessWriteBarrier) check(ctx context.Context) error {
	b.mu.Lock()
	open := b.open
	b.mu.Unlock()
	if !open {
		return ErrBusinessWritesClosed
	}
	return b.eligible(ctx)
}

func (s *Store) withBusinessWriteTx(ctx context.Context, fn func(*gorm.DB) error) error {
	return s.withBusinessWritePoolTx(ctx, s.db, fn)
}

// withBusinessWritePoolTx retains business admission through the supplied
// pool's actual commit or rollback. Strict local writers keep their own pool.
func (s *Store) withBusinessWritePoolTx(ctx context.Context, db *gorm.DB, fn func(*gorm.DB) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, endWork := s.bindMaintenanceWork(ctx, MaintenanceWorkTransaction)
	defer endWork()
	b := s.businessWriteBarrier
	if b != nil {
		release, err := b.acquire()
		if err != nil {
			return err
		}
		defer release() // Hold through GORM's actual commit or rollback.
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if b != nil {
			if err := b.check(ctx); err != nil {
				return err
			}
		}
		if err := requireBusinessServingMaintenancePhase(ctx, tx); err != nil {
			return err
		}
		if err := s.requireBusinessWriterAdmitted(ctx, tx); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		if err := readBusinessServingMaintenancePhase(ctx, tx); err != nil {
			return err
		}
		if b != nil {
			return b.check(ctx)
		}
		return nil
	})
}
