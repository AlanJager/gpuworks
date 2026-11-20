//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type heldBusinessGeneration struct {
	stopping atomic.Bool
	done     chan struct{}
	err      error
}

func (g *heldBusinessGeneration) BeginStop()     { g.stopping.Store(true) }
func (g *heldBusinessGeneration) Stopping() bool { return g.stopping.Load() }
func (g *heldBusinessGeneration) Wait(ctx context.Context) error {
	select {
	case <-g.done:
		return g.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestClosedBusinessCompletedDrainErrorReleasesOwner(t *testing.T) {
	drainErr := errors.New("member listener already stopped")
	generation := &heldBusinessGeneration{done: make(chan struct{}), err: drainErr}
	close(generation.done)
	a := &App{}
	if err := a.retainClosedBusiness(generation); err != nil {
		t.Fatal(err)
	}
	if err := a.stopClosedBusiness(t.Context()); !errors.Is(err, drainErr) || a.currentClosedBusiness() != nil {
		t.Fatalf("completed drain was retained after cleanup error: %v", err)
	}
}

func TestClosedBusinessDrainRetainsStoreAcrossCloseTimeout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	generation := &heldBusinessGeneration{done: make(chan struct{})}
	a := &App{store: st, ha: &memberRuntime{}}
	if err := a.retainClosedBusiness(generation); err != nil {
		t.Fatal(err)
	}
	if err := a.closeWithEligibilityTimeout(10 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close released a draining business generation: %v", err)
	}
	if !generation.Stopping() || a.currentClosedBusiness() != generation {
		t.Fatal("timed-out generation lost its retained owner")
	}
	if err := st.DB().Exec("SELECT 1").Error; err != nil {
		t.Fatalf("Close released Store before business drain: %v", err)
	}
	close(generation.done)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a.currentClosedBusiness() != nil {
		t.Fatal("drained generation was retained after Close")
	}
}

// Caller cancellation is normal; an actual retained-owner timeout is not.
func TestJoinedLifecycleReportsHeldBusinessShutdownStage(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "business.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	generation := &heldBusinessGeneration{done: make(chan struct{})}
	a := &App{store: st, memberState: memberState{legacyClosed: true}}
	if err := a.retainClosedBusiness(generation); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			close(generation.done)
		}
		_ = a.Close()
	}()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = a.runJoinedMaterialLifecycle(ctx, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stop joined business generation") {
		t.Fatalf("shutdown deadline lost stage or cause: %v", err)
	}
	if a.currentClosedBusiness() != generation || !generation.Stopping() {
		t.Fatal("timeout lost retained owner")
	}
	if err := st.DB().Exec("SELECT 1").Error; err != nil {
		t.Fatalf("timeout released Store: %v", err)
	}
	close(generation.done)
	released = true
	if err := a.runJoinedMaterialLifecycle(ctx, time.Minute); err != nil {
		t.Fatalf("drained cancellation failed: %v", err)
	}
	if a.currentClosedBusiness() != nil {
		t.Fatal("completed owner retained")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
