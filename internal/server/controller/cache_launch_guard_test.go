package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The reservation hook is after every rank's readiness lookup and before
// CreateInstances persists the model-file readers: exactly the vulnerable
// interval between a ready file being chosen and becoming protected.
type cacheLaunchBarrier struct {
	*recordingInferenceNodeHandler
	entered chan string
	resume  chan struct{}
	failure error
}

func (b *cacheLaunchBarrier) BeforeCreate(ctx context.Context, req provider.InferenceNodeCreateRequest) error {
	select {
	case b.entered <- req.NodeUUID:
	default:
	}
	select {
	case <-b.resume:
		return b.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}

func cacheGuardFixture(t *testing.T, pd bool) (*Controller, *store.Deployment, []*store.Node) {
	t.Helper()
	if pd {
		f := newPDFixture(t, t.Name(), twoPrefillOneDecode)
		return f.ctrl, f.dep, []*store.Node{f.nodes["n1"], f.nodes["n2"]}
	}
	s, dep, nodes, hub := twoNodeGangFixture(t, t.Name())
	return New(s, hub, nil), dep, nodes
}

func TestCacheLaunchPinsReadyFilesUntilReadersPersist(t *testing.T) {
	for _, pd := range []bool{false, true} {
		name := "ordinary"
		if pd {
			name = "pd"
		}
		for _, outcome := range []string{"success", "reservation_error", "cancelled"} {
			t.Run(name+"/"+outcome, func(t *testing.T) {
				c, dep, nodes := cacheGuardFixture(t, pd)
				barrier := &cacheLaunchBarrier{recordingInferenceNodeHandler: &recordingInferenceNodeHandler{}, entered: make(chan string, 1), resume: make(chan struct{})}
				if outcome == "reservation_error" {
					barrier.failure = errors.New("reservation rejected")
				}
				c.SetInferenceNodeLifecycleHandler(barrier)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- c.reconcileDeployment(ctx, dep.ID) }()
				var enteringNode string
				select {
				case enteringNode = <-barrier.entered:
				case err := <-done:
					t.Fatalf("launch did not reach readiness barrier: %v", err)
				case <-time.After(10 * time.Second):
					t.Fatal("launch stalled before readiness barrier")
				}
				// PD units pin only their own node, independently of the
				// other units' role ordering.
				pinned := nodes
				if pd {
					for _, node := range nodes {
						if node.UUID == enteringNode {
							pinned = []*store.Node{node}
						}
					}
				}
				for _, node := range pinned {
					if unlock, ok := c.store.TryEvictNodeCache(node.ID); ok {
						unlock()
						t.Fatalf("ready-but-unpersisted rank on %s allowed eviction", node.UUID)
					}
				}
				if outcome == "cancelled" {
					cancel()
				} else {
					close(barrier.resume)
				}
				select {
				case err := <-done:
					if (err != nil) != (outcome != "success") {
						t.Fatalf("reconcile error = %v, outcome %s", err, outcome)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("launch did not finish")
				}
				for _, node := range nodes {
					unlock, ok := c.store.TryEvictNodeCache(node.ID)
					if !ok {
						t.Fatalf("launch leaked node %s pin", node.UUID)
					}
					unlock()
				}
				var readers int64
				if err := c.store.DB().Model(&store.InstanceModelFile{}).Count(&readers).Error; err != nil {
					t.Fatal(err)
				}
				if (readers > 0) != (outcome == "success") {
					t.Fatalf("persisted readers = %d for %s", readers, outcome)
				}
			})
		}
	}
}

func TestCacheEvictionPostponesLaunchWithoutBlockingQueue(t *testing.T) {
	for _, pd := range []bool{false, true} {
		name := "ordinary"
		if pd {
			name = "pd"
		}
		t.Run(name, func(t *testing.T) {
			c, dep, nodes := cacheGuardFixture(t, pd)
			var releases []func()
			for _, node := range nodes {
				release, ok := c.store.TryEvictNodeCache(node.ID)
				if !ok {
					t.Fatal("could not hold eviction")
				}
				defer release()
				releases = append(releases, release)
			}
			done := make(chan error, 1)
			go func() { done <- c.reconcileDeployment(t.Context(), dep.ID) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("eviction blocked the deployment queue")
			}
			insts, err := c.store.InstancesForDeployment(dep.ID)
			if err != nil || len(insts) != 0 {
				t.Fatalf("launch crossed eviction: instances=%d err=%v", len(insts), err)
			}
			c.q.mu.Lock()
			_, retry := c.q.pending[depKey(dep.ID)]
			c.q.mu.Unlock()
			if !retry {
				t.Fatal("postponed launch was not requeued")
			}
			for _, release := range releases {
				release()
			}
			if err := c.reconcileDeployment(t.Context(), dep.ID); err != nil {
				t.Fatal(err)
			}
			insts, err = c.store.InstancesForDeployment(dep.ID)
			if err != nil || len(insts) == 0 {
				t.Fatalf("launch did not recover after eviction: instances=%d err=%v", len(insts), err)
			}
		})
	}
}
