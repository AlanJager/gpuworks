//go:build linux && dqlite

package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/cluster"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type classifiedJoinedSession struct {
	classify func(context.Context, *bus.Bus) (*store.Store, bool, error)
	calls    atomic.Int32
	closes   atomic.Int32
}

func (s *classifiedJoinedSession) OpenClassifiedStore(ctx context.Context, b *bus.Bus) (*store.Store, bool, error) {
	s.calls.Add(1)
	return s.classify(ctx, b)
}

func (s *classifiedJoinedSession) Close() error { s.closes.Add(1); return nil }

// These classification fixtures have no native quorum reader. The formal
// material router may retain its facts endpoint, but it must stay unavailable.
func (s *classifiedJoinedSession) ReadLocalQuorumFactsForCluster(ctx context.Context, _ store.ClusterIdentity, _ string) (bool, store.MaintenancePhaseState, []store.ClusterAdmission, cluster.QuorumProof, error) {
	err := ctx.Err()
	if err == nil {
		err = errors.New("classification fixture has no native quorum facts")
	}
	return false, store.MaintenancePhaseState{}, nil, cluster.QuorumProof{}, err
}

func joinedClassificationApp(t *testing.T, s joinedRaftSession) *App {
	t.Helper()
	gate := newBusinessHandlerGate()
	gate.requireMaterialEligibility()
	return &App{cfg: Config{Addr: "127.0.0.1:0", DataDir: t.TempDir()}, log: slog.New(slog.NewTextHandler(io.Discard, nil)), http: &http.Server{Handler: gate}, ha: &memberRuntime{}, memberState: memberState{joinedClosed: true, legacyClosed: true, joinedRaft: s, businessHTTP: gate}}
}

func assertJoinedWaitingHTTP(t *testing.T, a *App) {
	t.Helper()
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(3 * time.Second)
	for a.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	for _, path := range []string{"/healthz", "/api/v1/healthz", "/v1/models"} {
		response, err := client.Get("http://" + a.Addr() + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		want := http.StatusServiceUnavailable
		if path == "/healthz" {
			want = http.StatusOK
		}
		if response.StatusCode != want {
			t.Fatalf("%s status=%d, want=%d", path, response.StatusCode, want)
		}
	}
}

func TestJoinedClassificationHealthBeforeQuorumAndCancellationDrains(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := &classifiedJoinedSession{classify: func(ctx context.Context, b *bus.Bus) (*store.Store, bool, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		return nil, false, ctx.Err()
	}}
	a := joinedClassificationApp(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-entered
	assertJoinedWaitingHTTP(t, a)
	cancel()
	<-canceled
	select {
	case err := <-done:
		t.Fatalf("Run returned before original classifier drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if s.closes.Load() != 0 {
		t.Fatal("Run released original lease before classifier returned")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.store != nil || a.enrollment != nil {
		t.Fatal("canceled quorum wait assembled material services")
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v original session closes=%d", err, s.closes.Load())
	}
}

func TestJoinedClassificationSuccessfulTransferBeforeCancellationRetainsOwner(t *testing.T) {
	var returned *store.Store
	var originalBus *bus.Bus
	ctx, cancel := context.WithCancel(t.Context())
	s := &classifiedJoinedSession{classify: func(ctx context.Context, b *bus.Bus) (*store.Store, bool, error) {
		originalBus = b
		var err error
		returned, err = store.Open(filepath.Join(t.TempDir(), "owned.db"), b)
		if err != nil {
			return nil, false, err
		}
		cancel()
		return returned, false, nil
	}}
	a := joinedClassificationApp(t, s)
	if err := a.classifyJoinedStore(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a.store != returned || a.store.Bus != originalBus || a.joinedRaft != s || a.legacyClosed {
		t.Fatal("successful transfer discarded original Store, Bus or session")
	}
	var one int
	if err := returned.DB().Raw("SELECT 1").Scan(&one).Error; err != nil || one != 1 {
		t.Fatalf("transferred Store prematurely closed: one=%d err=%v", one, err)
	}
	if err := a.classifyJoinedStore(t.Context()); err != nil || s.calls.Load() != 1 {
		t.Fatalf("transferred session reused: calls=%d err=%v", s.calls.Load(), err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := returned.DB().Raw("SELECT 1").Scan(&one).Error; err == nil || s.closes.Load() != 1 {
		t.Fatal("Close did not release transferred resources")
	}
}

func TestJoinedClassificationLegacyAndCurrentFrozenKeepHealthLive(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "current-frozen"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			var st *store.Store
			s := &classifiedJoinedSession{classify: func(ctx context.Context, b *bus.Bus) (*store.Store, bool, error) {
				var err error
				st, err = store.Open(filepath.Join(t.TempDir(), "classified.db"), b)
				if err != nil {
					return nil, false, err
				}
				if err := st.DB().Exec("UPDATE maintenance_phase SET phase = 'frozen', revision = revision + 1 WHERE id = 1").Error; err != nil {
					return nil, false, err
				}
				close(entered)
				return st, legacy, nil
			}}
			a := joinedClassificationApp(t, s)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- a.Run(ctx) }()
			<-entered
			assertJoinedWaitingHTTP(t, a)
			select {
			case err := <-done:
				t.Fatalf("closed phase stopped liveness: %v", err)
			default:
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if a.store != st || a.legacyClosed != legacy || a.enrollment != nil || s.calls.Load() != 1 {
				t.Fatal("classification lost identity or initialized closed-phase materials")
			}
			if err := fencedBusinessWrite(st); !errors.Is(err, store.ErrBusinessWritesClosed) {
				t.Fatalf("classified Store admitted direct business write: %v", err)
			}
			if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "material-replicas")); !os.IsNotExist(err) {
				t.Fatalf("closed phase initialized files: %v", err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJoinedClassificationInvalidSchemaPropagatesWithoutAssembly(t *testing.T) {
	invalid := store.ErrSchemaIncompatible
	s := &classifiedJoinedSession{classify: func(context.Context, *bus.Bus) (*store.Store, bool, error) {
		return nil, false, invalid
	}}
	a := joinedClassificationApp(t, s)
	if err := a.Run(t.Context()); !errors.Is(err, invalid) {
		t.Fatal(err)
	}
	if a.store != nil || a.enrollment != nil || s.calls.Load() != 1 || s.closes.Load() != 0 {
		t.Fatal("invalid schema assembled services or released original owner")
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatal(err)
	}
}

func TestJoinedClassificationCancellationPreservesJoinedCleanupError(t *testing.T) {
	cleanupErr := errors.New("material directory cleanup failed")
	entered := make(chan struct{})
	s := &classifiedJoinedSession{classify: func(ctx context.Context, _ *bus.Bus) (*store.Store, bool, error) {
		close(entered)
		<-ctx.Done()
		return nil, false, errors.Join(ctx.Err(), cleanupErr)
	}}
	a := joinedClassificationApp(t, s)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, cleanupErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lifecycle suppressed joined cleanup failure: %v", err)
	}
	if s.closes.Load() != 0 {
		t.Fatal("failed cleanup released original session during Run")
	}
	if err := a.Close(); err != nil || s.closes.Load() != 1 {
		t.Fatalf("Close=%v closes=%d", err, s.closes.Load())
	}
}
