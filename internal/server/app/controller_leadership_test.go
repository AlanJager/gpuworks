//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type roleObservation struct {
	local bool
	err   error
}

func TestControllerSupervisorDrainsBeforeReobservingLeadership(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observations := make(chan roleObservation)
	queried := make(chan struct{}, 8)
	var starts atomic.Int32
	started := make(chan int32, 4)
	stopped := make(chan struct{}, 4)
	release := make(chan struct{})
	run := func(ctx context.Context) {
		n := starts.Add(1)
		started <- n
		<-ctx.Done()
		stopped <- struct{}{}
		if n == 1 {
			<-release
		}
	}
	observe := func(ctx context.Context) (bool, error) {
		queried <- struct{}{}
		select {
		case result := <-observations:
			return result.local, result.err
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, observe, run, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Second, nil, nil)
	}()
	released := false
	t.Cleanup(func() {
		cancel()
		if !released {
			close(release)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("supervisor did not drain")
		}
	})
	next := func(role roleObservation) {
		t.Helper()
		select {
		case <-queried:
		case <-time.After(2 * time.Second):
			t.Fatal("no observation")
		}
		select {
		case observations <- role:
		case <-time.After(2 * time.Second):
			t.Fatal("observation did not consume result")
		}
	}
	next(roleObservation{}) // follower must stay idle.
	next(roleObservation{local: true})
	select {
	case n := <-started:
		if n != 1 {
			t.Fatal(n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not start")
	}
	next(roleObservation{local: true})
	next(roleObservation{local: false})
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("lost role did not cancel")
	}
	// The old worker is still active. A new local observation cannot be consumed.
	select {
	case <-queried:
		t.Fatal("observed a new role before old controller drained")
	default:
	}
	if starts.Load() != 1 {
		t.Fatal("overlapping controller lifetimes")
	}
	close(release)
	released = true
	next(roleObservation{local: true})
	select {
	case n := <-started:
		if n != 2 {
			t.Fatal(n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not restart")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	if starts.Load() != 2 {
		t.Fatal("duplicate controller start")
	}
}

func TestControllerSupervisorRejectsFailedAndExpiredObservations(t *testing.T) {
	for _, scenario := range []string{"error", "deadline"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls, starts atomic.Int32
			exited := make(chan struct{})
			observe := func(ctx context.Context) (bool, error) {
				if calls.Add(1) == 1 {
					return true, nil
				}
				if scenario == "deadline" {
					<-ctx.Done()
					return true, nil
				} // Late positive is not authority.
				return true, errors.New("leader lookup unavailable")
			}
			run := func(ctx context.Context) { starts.Add(1); <-ctx.Done(); close(exited) }
			done := make(chan struct{})
			go func() {
				defer close(done)
				superviseController(ctx, observe, run, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, 10*time.Millisecond, nil, nil)
			}()
			t.Cleanup(func() { cancel(); <-done })
			select {
			case <-exited:
			case <-time.After(2 * time.Second):
				t.Fatal("failed observation did not cancel controller")
			}
			cancel()
			<-done
			if starts.Load() != 1 {
				t.Fatalf("started after failed observation: %d", starts.Load())
			}
		})
	}
}

func TestControllerSupervisorCancellationWaitsForWorker(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, func(context.Context) (bool, error) { return true, nil }, func(ctx context.Context) { close(started); <-ctx.Done(); close(canceled); <-release }, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, time.Second, nil, nil)
	}()
	released := false
	t.Cleanup(func() {
		cancel()
		if !released {
			close(release)
		}
		<-done
	})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("controller missing")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("worker not canceled")
	}
	select {
	case <-done:
		t.Fatal("shutdown skipped worker drain")
	default:
	}
	close(release)
	released = true
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("drained worker not released")
	}
}

func TestControllerSupervisorRequiresNewObservationAfterUnexpectedExit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls, starts atomic.Int32
	observe := func(context.Context) (bool, error) {
		if calls.Add(1) == 1 {
			return true, nil
		}
		cancel()
		return false, nil
	}
	superviseController(ctx, observe, func(context.Context) { starts.Add(1) }, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Second, nil, nil)
	if starts.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("starts=%d calls=%d", starts.Load(), calls.Load())
	}
}

func TestControllerSupervisorWaitsForCommittedAcquisition(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "acquisition.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.DB().Exec(`CREATE TRIGGER reject_owner BEFORE INSERT ON settings
		WHEN NEW.key = 'scheduling.owner' BEGIN SELECT RAISE(ABORT, 'injected owner failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	attempts := make(chan error, 8)
	started := make(chan uint64, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, st.LocalSchedulingLeader, func(ctx context.Context) {
			generation, err := st.SchedulingGeneration(ctx)
			if err != nil {
				t.Error(err)
			}
			started <- generation
			<-ctx.Done()
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), 10*time.Millisecond, time.Second, func(leader bool) error {
			err := st.BeginSchedulingLeadership(ctx)
			select {
			case attempts <- err:
			default:
			}
			return err
		}, nil)
	}()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case err := <-attempts:
		if err == nil {
			t.Fatal("injected transaction succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leadership acquisition was not attempted")
	}
	select {
	case generation := <-started:
		t.Fatalf("controller started before acquisition at generation %d", generation)
	default:
	}
	if generation, err := st.SchedulingGeneration(t.Context()); err != nil || generation != 0 {
		t.Fatalf("failed acquisition changed generation=%d err=%v", generation, err)
	}
	if err := st.DB().Exec("DROP TRIGGER reject_owner").Error; err != nil {
		t.Fatal(err)
	}
	select {
	case generation := <-started:
		if generation != 1 {
			t.Fatalf("controller started at generation %d", generation)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("controller did not start after committed acquisition")
	}
	if generation, err := st.SchedulingGeneration(t.Context()); err != nil || generation != 1 {
		t.Fatalf("acquisition generation=%d err=%v", generation, err)
	}
}

func TestControllerStaysClosedUntilTheOldMemberIsIsolated(t *testing.T) {
	state := BeginDisasterRecovery()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queried := make(chan struct{}, 1)
	release := make(chan struct{})
	var starts atomic.Int32
	started := make(chan struct{}, 1)
	observe := func(ctx context.Context) (bool, error) {
		select {
		case queried <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, observe, func(context.Context) {
			if starts.Add(1) == 1 {
				started <- struct{}{}
			}
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Second, nil, func() bool {
			return schedulingAllowed(func() *DisasterRecovery { return &state })
		})
	}()
	t.Cleanup(func() {
		cancel()
		close(release)
		<-done
	})
	waitQuery := func() {
		t.Helper()
		select {
		case <-queried:
		case <-time.After(2 * time.Second):
			t.Fatal("supervisor did not observe")
		}
	}
	waitQuery()
	release <- struct{}{}
	waitQuery()
	state = NoteClusterIDChange(state, "replacement-cluster")
	release <- struct{}{}
	waitQuery()
	if starts.Load() != 0 || state.Isolated || state.Scheduling {
		t.Fatalf("scheduling resumed without isolation: starts=%d state=%+v", starts.Load(), state)
	}
	state = ConfirmMemberIsolated(state)
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("isolated recovery did not start scheduling")
	}
	if !state.Isolated || !state.Scheduling || starts.Load() != 1 {
		t.Fatalf("isolation did not open scheduling: starts=%d state=%+v", starts.Load(), state)
	}
}

func TestControllerLeadershipLossFencesTheOldPlacement(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "sched.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queried := make(chan struct{}, 1)
	release := make(chan bool)
	observe := func(ctx context.Context) (bool, error) {
		select {
		case queried <- struct{}{}:
		default:
		}
		select {
		case local := <-release:
			return local, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	started := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		superviseController(ctx, observe, func(ctx context.Context) {
			select {
			case started <- struct{}{}:
			default:
			}
			<-ctx.Done()
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Millisecond, time.Second, func(leader bool) error {
			return st.NoteSchedulingLeadership(context.Background(), leader)
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-queried:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not observe")
	}
	release <- true
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("leader did not start")
	}
	select {
	case <-queried:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor did not observe the role loss")
	}
	release <- false
	deadline := time.Now().Add(2 * time.Second)
	for {
		generation, err := st.SchedulingGeneration(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if generation == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leadership loss left generation %d", generation)
		}
		time.Sleep(5 * time.Millisecond)
	}
	err = st.WithSchedulingTx(context.Background(), func(tx *store.SchedulingTx) error {
		return tx.CreateInstance(&store.Instance{UUID: "old-placement", NodeID: 1})
	})
	if !errors.Is(err, store.ErrStaleSchedulingGeneration) {
		t.Fatalf("old placement committed after leadership loss: %v", err)
	}
}
