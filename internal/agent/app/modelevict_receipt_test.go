package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func cacheEvictParams(path, token string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"path": path, "eviction_id": token})
	return raw
}

func newCacheEvictionRuntime(t *testing.T) *NodeRuntime {
	t.Helper()
	n, _ := newEvictionRuntime(t)
	n.stateDir = t.TempDir()
	return n
}

func cacheEvictionModel(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "weights"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCacheEvictionContent(t *testing.T, dir, content string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, "weights"))
	if err != nil || string(got) != content {
		t.Fatalf("weights in %s = %q, %v; want %q", dir, got, err, content)
	}
}

func cacheEvictionRoots(t *testing.T, n *NodeRuntime) (*os.Root, *os.Root) {
	t.Helper()
	root, err := os.OpenRoot(n.ModelsRoot())
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.OpenRoot(n.stateDir)
	if err != nil {
		root.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close(); state.Close() })
	return root, state
}

func TestCacheEvictionReceiptSurvivesRestartAndDelayedDuplicate(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	cacheEvictionModel(t, target, "original")
	params := cacheEvictParams(target, "operation-1")
	result, err := n.OnRPC(context.Background(), proto.RPCCacheEvict, params)
	if err != nil {
		t.Fatal(err)
	}
	got := result.(map[string]any)
	if got["removed"] != true || got["path"] != target || got["eviction_id"] != "operation-1" {
		t.Fatalf("result = %#v", got)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original target remains: %v", err)
	}
	cacheEvictionModel(t, target, "replacement")
	// This is a fresh process-local runtime: only disk state is reused.
	restarted := newCacheEvictionRuntime(t)
	restarted.modelsDir, restarted.stateDir = n.modelsDir, n.stateDir
	canceled := false
	restarted.downloading["new"] = &modelDownload{target: target, cancel: func() { canceled = true }, done: make(chan struct{})}
	if _, err := restarted.OnRPC(context.Background(), proto.RPCCacheEvict, params); err != nil {
		t.Fatal(err)
	}
	if canceled {
		t.Fatal("duplicate canceled the replacement download")
	}
	assertCacheEvictionContent(t, target, "replacement")
	// Root configuration may be broader after restart. Receipt lookup is tied
	// to the absolute target and stable state, not the current storage root.
	restarted.modelsDir = filepath.Dir(n.ModelsRoot())
	if _, err := restarted.rpcCacheEvict(context.Background(), params); err != nil {
		t.Fatal(err)
	}
	assertCacheEvictionContent(t, target, "replacement")
	if canceled {
		t.Fatal("root change caused duplicate to cancel the replacement download")
	}
	delete(restarted.downloading, "new")
	if _, err := restarted.rpcCacheEvict(context.Background(), cacheEvictParams(target, "operation-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new operation did not evict replacement: %v", err)
	}
}

func TestCacheEvictionPartialRemovalRetriesOnlyDetachedData(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	cacheEvictionModel(t, target, "original")
	if err := os.WriteFile(filepath.Join(target, "index"), []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, state := cacheEvictionRoots(t, n)
	receipt := cacheEvictionReceipt(target, "partial")
	injected := errors.New("partial removal")
	err := removeCacheWithReceipt(context.Background(), root, state, "org/model", receipt, func(data string) error {
		if err := root.Remove(filepath.Join(data, "index")); err != nil {
			t.Fatal(err)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("partial removal = %v", err)
	}
	data := filepath.Join(n.ModelsRoot(), "org", receipt, "data")
	assertCacheEvictionContent(t, data, "original")
	cacheEvictionModel(t, target, "replacement")
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "partial")); err != nil {
		t.Fatal(err)
	}
	assertCacheEvictionContent(t, target, "replacement")
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("detached bytes remain: %v", err)
	}
}

func TestCacheEvictionRecoversRenameBeforeReceipt(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	cacheEvictionModel(t, target, "original")
	receipt := cacheEvictionReceipt(target, "crash-after-rename")
	journal := filepath.Join(filepath.Dir(target), receipt)
	if err := os.MkdirAll(journal, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(target, filepath.Join(journal, "data")); err != nil {
		t.Fatal(err)
	}
	cacheEvictionModel(t, target, "replacement")
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "crash-after-rename")); err != nil {
		t.Fatal(err)
	}
	assertCacheEvictionContent(t, target, "replacement")
	if _, err := os.Stat(filepath.Join(n.stateDir, receipt)); err != nil {
		t.Fatalf("durable receipt missing: %v", err)
	}
}

func TestCacheEvictionDoesNotDeleteWithoutWritableReceipt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses filesystem write permissions")
	}
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	cacheEvictionModel(t, target, "original")
	stateDir := filepath.Join(n.stateDir, cacheEvictionDirectory)
	if err := os.Mkdir(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "write-failure")); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("receipt write = %v", err)
	}
	receipt := cacheEvictionReceipt(target, "write-failure")
	data := filepath.Join(filepath.Dir(target), receipt, "data")
	assertCacheEvictionContent(t, data, "original")
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "write-failure")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry did not finish removal: %v", err)
	}
}

func TestCacheEvictionRejectsInvalidPathsAndJournals(t *testing.T) {
	for _, invalid := range []string{"", " ", ".", "../escape", "org/../../escape", ".gpuworks-evictions/receipt"} {
		t.Run("path="+invalid, func(t *testing.T) {
			n := newCacheEvictionRuntime(t)
			if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(invalid, "token")); err == nil {
				t.Fatalf("accepted path %q", invalid)
			}
		})
	}
	for _, token := range []string{"", " ", strings.Repeat("x", 257)} {
		n := newCacheEvictionRuntime(t)
		if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams("model", token)); err == nil {
			t.Fatalf("accepted token %q", token)
		}
	}
	for _, link := range []string{"target", "parent", "journal", "receipt"} {
		t.Run(link+"-symlink", func(t *testing.T) {
			n := newCacheEvictionRuntime(t)
			outside := t.TempDir()
			cacheEvictionModel(t, outside, "untouched")
			target := filepath.Join(n.ModelsRoot(), "org", "model")
			cacheEvictionModel(t, target, "original")
			var path string
			switch link {
			case "target":
				path = target
			case "parent":
				path = filepath.Dir(target)
			case "journal":
				path = filepath.Join(filepath.Dir(target), cacheEvictionDirectory)
			case "receipt":
				path = filepath.Join(n.stateDir, cacheEvictionDirectory)
			}
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "token")); err == nil {
				t.Fatal("accepted symlink")
			}
			assertCacheEvictionContent(t, outside, "untouched")
		})
	}
}

func TestCacheEvictionSerializesDuplicateRemoval(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "org", "model")
	cacheEvictionModel(t, target, "original")
	root, state := cacheEvictionRoots(t, n)
	receipt := cacheEvictionReceipt(target, "concurrent")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	first := make(chan error, 1)
	go func() {
		first <- n.removeModelPathWith(context.Background(), target, nil, func() error {
			return removeCacheWithReceipt(context.Background(), root, state, "org/model", receipt, func(data string) error {
				close(entered)
				<-release
				return root.RemoveAll(data)
			})
		})
	}()
	<-entered
	if n.dlMu.TryLock() {
		n.dlMu.Unlock()
		t.Fatal("removal does not hold download serialization lock")
	}
	second := make(chan error, 1)
	go func() {
		_, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "concurrent"))
		second <- err
	}()
	once.Do(func() { close(release) })
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
}

func TestCacheEvictionMissingPathAndCanceledContext(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "missing", "model")
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "missing")); err != nil {
		t.Fatal(err)
	}
	cacheEvictionModel(t, target, "new")
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "missing")); err != nil {
		t.Fatal(err)
	}
	assertCacheEvictionContent(t, target, "new")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.rpcCacheEvict(ctx, cacheEvictParams(target, "canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled RPC = %v", err)
	}
	assertCacheEvictionContent(t, target, "new")
}

func TestCacheEvictionJournalFailureBeforeRenamePreservesTarget(t *testing.T) {
	n := newCacheEvictionRuntime(t)
	target := filepath.Join(n.ModelsRoot(), "model")
	cacheEvictionModel(t, target, "original")
	if err := os.WriteFile(filepath.Join(n.stateDir, cacheEvictionDirectory), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "no-receipt")); err == nil {
		t.Fatal("accepted invalid state journal")
	}
	assertCacheEvictionContent(t, target, "original")
	// An authorized model directory must still never contain agent state.
	n.stateDir = filepath.Join(target, "agent-state")
	if err := os.Mkdir(n.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := n.rpcCacheEvict(context.Background(), cacheEvictParams(target, "state-under-model")); err == nil {
		t.Fatal("accepted removal of agent state")
	}
	assertCacheEvictionContent(t, target, "original")
}
