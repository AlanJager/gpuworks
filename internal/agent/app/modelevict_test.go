package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

type recordedModelStatuses struct {
	mu       sync.Mutex
	statuses []proto.ModelFileStatus
}

func (r *recordedModelStatuses) snapshot() []proto.ModelFileStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]proto.ModelFileStatus(nil), r.statuses...)
}

func newEvictionRuntime(t *testing.T) (*NodeRuntime, *recordedModelStatuses) {
	t.Helper()
	n := &NodeRuntime{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		modelsDir:   t.TempDir(),
		downloading: map[string]*modelDownload{},
		progress:    newProgressGate(),
	}
	recorded := &recordedModelStatuses{}
	n.Bind(func(kind string, value any) error {
		if kind == proto.TStatusModelFile {
			recorded.mu.Lock()
			recorded.statuses = append(recorded.statuses, value.(proto.ModelFileStatus))
			recorded.mu.Unlock()
		}
		return nil
	})
	return n, recorded
}

func evictParams(path string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"path": path})
	return raw
}

func (n *NodeRuntime) runningDownloads() int {
	n.dlMu.Lock()
	defer n.dlMu.Unlock()
	return len(n.downloading)
}

// The weight transfer of this fixture never finishes on its own: it holds
// every ranged request until the client goes away, like a registry serving a
// large file slowly.
func newHeldWeightHub(t *testing.T) (endpoint string, started, abandoned <-chan struct{}) {
	t.Helper()
	config := []byte(`{"model_type":"test"}`)
	weights := []byte(strings.Repeat("w", 64))
	startedCh, abandonedCh := make(chan struct{}), make(chan struct{})
	var startedOnce, abandonedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/org/model/tree/main":
			_, _ = fmt.Fprintf(w, `[{"type":"file","path":"config.json","size":%d},{"type":"file","path":"model.safetensors","size":%d}]`, len(config), len(weights))
		case "/org/model/resolve/main/config.json":
			http.ServeContent(w, r, "config.json", time.Time{}, strings.NewReader(string(config)))
		case "/org/model/resolve/main/model.safetensors":
			if r.Header.Get("Range") == "bytes=0-0" {
				http.ServeContent(w, r, "model.safetensors", time.Time{}, strings.NewReader(string(weights)))
				return
			}
			startedOnce.Do(func() { close(startedCh) })
			<-r.Context().Done()
			abandonedOnce.Do(func() { close(abandonedCh) })
		default:
			http.Error(w, "unexpected fixture request "+r.URL.String(), http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, startedCh, abandonedCh
}

func TestEvictStopsTheRunningDownloadBeforeRemovingIt(t *testing.T) {
	endpoint, started, abandoned := newHeldWeightHub(t)
	n, recorded := newEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	n.reconcileModelFile(context.Background(), proto.ModelFileTask{
		Key: "3-4", Source: "huggingface", SourceID: "org/model", Revision: "main",
		Endpoint: endpoint, TargetPath: "org/model", DesiredState: "present", Acquisition: proto.ModelAcquireDownload,
	})
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the weight transfer to start")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("download has not created its directory: %v", err)
	}

	result, err := n.rpcModelEvict(context.Background(), evictParams("org/model"))
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if got := result.(map[string]any); got["path"] != target || got["removed"] != true {
		t.Fatalf("evict result = %+v", got)
	}
	// The guarantees hold the moment evict returns, without waiting for them.
	if running := n.runningDownloads(); running != 0 {
		t.Fatalf("%d downloads still registered after evict", running)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model directory after evict: %v", err)
	}
	afterEvict := len(recorded.snapshot())
	select {
	case <-abandoned:
	case <-time.After(30 * time.Second):
		t.Fatal("the held transfer was never abandoned")
	}
	// Longer than the progress gate's interval, so a suppressed sample of the
	// canceled download would have been flushed by now.
	time.Sleep(statusProgressInterval + 500*time.Millisecond)
	statuses := recorded.snapshot()
	if len(statuses) != afterEvict {
		t.Fatalf("the canceled download reported after evict returned: %+v", statuses[afterEvict:])
	}
	for _, st := range statuses {
		if st.State != proto.MFDownloading {
			t.Fatalf("eviction surfaced as a download outcome: %+v", st)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model directory reappeared after evict: %v", err)
	}
}

// A writer that does not stop keeps its files: eviction gives up when its
// caller does and says so, instead of deleting under a live download.
func TestEvictFailsWhileAnOverlappingDownloadKeepsRunning(t *testing.T) {
	n, _ := newEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	canceled := 0
	n.downloading["5-6"] = &modelDownload{target: target, cancel: func() { canceled++ }, done: make(chan struct{})}

	for _, path := range []string{"org/model", "org"} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := n.rpcModelEvict(ctx, evictParams(path))
		cancel()
		if err == nil || !strings.Contains(err.Error(), "did not stop") || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("evict %q with a running download: err=%v", path, err)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("evict %q removed files a download still writes: %v", path, err)
		}
	}
	if canceled != 2 {
		t.Fatalf("running download canceled %d times, want once per eviction", canceled)
	}

	// A download elsewhere under the models root does not hold up this path.
	other := filepath.Join(n.ModelsRoot(), "other", "model")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := n.rpcModelEvict(context.Background(), evictParams("other/model")); err != nil {
		t.Fatalf("evict unrelated path: %v", err)
	}
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unrelated model directory after evict: %v", err)
	}
}

func TestEvictRejectsPathsOutsideTheModelsRoot(t *testing.T) {
	n, _ := newEvictionRuntime(t)
	root := n.ModelsRoot()
	outside := t.TempDir()
	for _, path := range []string{".", "../escape", "org/../../escape", outside, root} {
		_, err := n.rpcModelEvict(context.Background(), evictParams(path))
		if err == nil || !strings.Contains(err.Error(), "outside models root") {
			t.Errorf("evict %q: err=%v, want outside models root", path, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("directory outside the models root was touched: %v", err)
	}
	if _, err := n.rpcModelEvict(context.Background(), json.RawMessage(`{"path":"  "}`)); err == nil || !strings.Contains(err.Error(), "requires path") {
		t.Fatalf("evict without path: err=%v", err)
	}
}

func TestEvictReportsARemovalTheFilesystemRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the refusal cannot be produced")
	}
	n, _ := newEvictionRuntime(t)
	parent := filepath.Join(n.ModelsRoot(), "org")
	target := filepath.Join(parent, "model")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o755) })

	_, err := n.rpcModelEvict(context.Background(), evictParams("org/model"))
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("evict of a read-only model directory: err=%v, want permission denied", err)
	}
	if _, err := os.Stat(filepath.Join(target, "config.json")); err != nil {
		t.Fatalf("model file after refused evict: %v", err)
	}
}
