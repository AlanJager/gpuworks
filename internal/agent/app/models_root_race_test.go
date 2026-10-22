package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestConcurrentModelsRootReadsAndStateUpdatesAreRaceFree pins the models-root
// guard: the server can move the root through a state update while downloads,
// doctor and path-usage readers run on other goroutines. Before the guard this
// pair was a plain field read and write.
func TestConcurrentModelsRootReadsAndStateUpdatesAreRaceFree(t *testing.T) {
	root := t.TempDir()
	n := newNodeRuntime(slog.New(slog.NewTextHandler(io.Discard, nil)), nil, root, "none", metricsOnlyProber{}, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if root := n.ModelsRoot(); root == "" {
					return
				}
				_ = n.resolveModelPath("model/config.json")
			}
		}()
	}
	for i := 0; i < 200; i++ {
		n.OnStateUpdate(context.Background(), proto.StateUpdate{ModelsRoot: filepath.Join(root, fmt.Sprintf("models-%d", i))})
	}
	close(stop)
	wg.Wait()
	if got := n.ModelsRoot(); got != filepath.Join(root, "models-199") {
		t.Fatalf("models root = %q, want the last state update", got)
	}
}
