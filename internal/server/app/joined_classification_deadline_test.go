//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The pinned native SDK returns its leader sentinel when its request context
// expires, rather than wrapping that context's DeadlineExceeded.
func TestJoinedClassificationExpiredSDKAttemptRetriesWithOriginalOwner(t *testing.T) {
	sdkFailure := errors.New("no available dqlite leader server found")
	entered, classified := make(chan struct{}), make(chan struct{})
	returned, err := store.Open(filepath.Join(t.TempDir(), "classified.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = returned.Close() })
	if err := returned.DB().Exec("UPDATE maintenance_phase SET phase = 'frozen', revision = revision + 1 WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	s := &classifiedJoinedSession{}
	s.classify = func(ctx context.Context, b *bus.Bus) (*store.Store, bool, error) {
		if s.calls.Load() == 1 {
			close(entered)
			<-ctx.Done()
			return nil, false, sdkFailure
		}
		returned.Bus = b
		close(classified)
		return returned, false, nil
	}
	a := joinedClassificationApp(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-entered
	assertJoinedWaitingHTTP(t, a)
	select {
	case <-classified:
	case err := <-done:
		t.Fatalf("expired native attempt terminated waiting App: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("expired native attempt did not retry")
	}
	assertJoinedWaitingHTTP(t, a)
	if s.closes.Load() != 0 {
		t.Fatal("classification retry released the original runtime owner")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.store != returned || a.joinedRaft != s || s.calls.Load() != 2 || a.enrollment != nil {
		t.Fatal("retry replaced original owners or assembled frozen-phase materials")
	}
	if err := fencedBusinessWrite(returned); !errors.Is(err, store.ErrBusinessWritesClosed) {
		t.Fatalf("classified frozen Store admitted business write: %v", err)
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v runtime closes=%d", err, s.closes.Load())
	}
}

func TestJoinedClassificationExpiredCallerPreservesSDKAndDeadlineErrors(t *testing.T) {
	sdkFailure := errors.New("no available dqlite leader server found")
	s := &classifiedJoinedSession{classify: func(ctx context.Context, _ *bus.Bus) (*store.Store, bool, error) {
		<-ctx.Done()
		return nil, false, sdkFailure
	}}
	a := joinedClassificationApp(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	err := a.classifyJoinedStore(ctx)
	if !errors.Is(err, sdkFailure) || !errors.Is(err, context.DeadlineExceeded) || s.calls.Load() != 1 {
		t.Fatalf("expired caller: err=%v attempts=%d", err, s.calls.Load())
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v runtime closes=%d", err, s.closes.Load())
	}
}

func TestJoinedClassificationUnexpiredSDKFailureRemainsFatal(t *testing.T) {
	sdkFailure := errors.New("no available dqlite leader server found")
	s := &classifiedJoinedSession{classify: func(context.Context, *bus.Bus) (*store.Store, bool, error) {
		return nil, false, sdkFailure
	}}
	a := joinedClassificationApp(t, s)
	err := a.classifyJoinedStore(t.Context())
	if err != sdkFailure || errors.Is(err, context.DeadlineExceeded) || s.calls.Load() != 1 {
		t.Fatalf("unexpired failure masked or retried: err=%v attempts=%d", err, s.calls.Load())
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v runtime closes=%d", err, s.closes.Load())
	}
}

func TestJoinedClassificationCanceledCallerDoesNotRetrySDKFailure(t *testing.T) {
	sdkFailure := errors.New("no available dqlite leader server found")
	ctx, cancel := context.WithCancel(t.Context())
	s := &classifiedJoinedSession{classify: func(attempt context.Context, _ *bus.Bus) (*store.Store, bool, error) {
		cancel()
		<-attempt.Done()
		return nil, false, sdkFailure
	}}
	a := joinedClassificationApp(t, s)
	err := a.classifyJoinedStore(ctx)
	if err != sdkFailure || errors.Is(err, context.DeadlineExceeded) || s.calls.Load() != 1 {
		t.Fatalf("canceled caller masked or retried: err=%v attempts=%d", err, s.calls.Load())
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v runtime closes=%d", err, s.closes.Load())
	}
}
