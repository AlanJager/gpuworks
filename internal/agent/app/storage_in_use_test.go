package app

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/engine"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// What keeps a hung mount from being forced off, source by source: each one
// alone makes its own storage in use and leaves the other storage free.
func TestStorageInUseCountsEverythingThatReadsOrWritesTheStorage(t *testing.T) {
	root, other := filepath.Join(t.TempDir(), "nfs"), filepath.Join(t.TempDir(), "other")
	fresh := func() *NodeRuntime {
		n := &NodeRuntime{downloading: map[string]*modelDownload{}, modelSyncs: map[string]*activeModelSync{}}
		n.storage = &storageRuntime{users: storageUsers(n)}
		return n
	}
	check := func(t *testing.T, n *NodeRuntime, want bool) {
		t.Helper()
		if got := n.storage.inUse(1, root); got != want {
			t.Fatalf("inUse(%s) = %v, want %v", root, got, want)
		}
		if n.storage.inUse(2, other) {
			t.Fatalf("a storage nothing touches is in use")
		}
		if n.storage.inUse(3, root+"-sibling") {
			t.Fatalf("a directory that only shares the prefix is in use")
		}
	}
	t.Run("nothing", func(t *testing.T) { check(t, fresh(), false) })
	t.Run("download or copy in flight", func(t *testing.T) {
		n := fresh()
		n.downloading["k"] = &modelDownload{target: filepath.Join(root, "org", "model"), copy: true}
		check(t, n, true)
		delete(n.downloading, "k")
		check(t, n, false)
	})
	t.Run("peer transfer receiving", func(t *testing.T) {
		n := fresh()
		n.modelSyncs["k"] = &activeModelSync{key: "k", target: filepath.Join(root, "org", "model")}
		check(t, n, true)
	})
	t.Run("peer transfer sending", func(t *testing.T) {
		n := fresh()
		release := n.storage.holdRead(filepath.Join(root, "org", "model"))
		again := n.storage.holdRead(filepath.Join(root, "org", "model"))
		check(t, n, true)
		release()
		release() // releasing twice does not release the other holder
		check(t, n, true)
		again()
		check(t, n, false)
	})
	t.Run("instances not known yet", func(t *testing.T) {
		n := fresh()
		// The control plane has not sent this agent its instances: whether
		// one of them mounts the storage cannot be told, so it is in use.
		n.engine = engine.New(nil, nil, nil, t.TempDir())
		if !n.storage.inUse(1, root) || !n.storage.inUse(2, other) {
			t.Fatal("a storage is free while the agent's instances are unknown")
		}
	})
	t.Run("instance in any state", func(t *testing.T) {
		for name, spec := range map[string]proto.InstanceDesiredState{
			"asleep, model on it":      {UUID: "a", DesiredState: "running", DesiredSleep: 2, ModelPath: filepath.Join(root, "m")},
			"wanted, component mount":  {UUID: "b", DesiredState: "running", ModelPath: "/elsewhere/m", ResolvedLaunch: proto.ResolvedLaunch{Mounts: []proto.MountSpec{{Source: filepath.Join(root, "vae"), Target: "/vae"}}}},
			"wanted, the storage root": {UUID: "c", DesiredState: "running", ModelPath: root},
		} {
			t.Run(name, func(t *testing.T) {
				n := fresh()
				driver := recording.New(filepath.Join(t.TempDir(), "rec.jsonl"))
				n.engine = engine.New(driver, func(proto.InstanceStatus) {}, slog.New(slog.DiscardHandler), t.TempDir())
				// The sync is all this needs: with its context already ended
				// the engine records the instance and starts nothing.
				ended, cancel := context.WithCancel(context.Background())
				cancel()
				n.engine.SetDesiredFull(ended, []proto.InstanceDesiredState{spec})
				check(t, n, true)
			})
		}
	})
}

// The mount manager asks the node, not only the awake instances.
func TestStorageRuntimeForcesNoMountTheNodeUses(t *testing.T) {
	r := newStorageTestRuntime(t)
	if r.storage == nil {
		t.Fatal("no storage runtime")
	}
	n := newNodeRuntime(r.log, nil, filepath.Join(t.TempDir(), "models"), "none", metricsOnlyProber{}, nil)
	root := filepath.Join(t.TempDir(), "nfs")
	n.dlMu.Lock()
	n.downloading["k"] = &modelDownload{target: filepath.Join(root, "m")}
	n.dlMu.Unlock()
	if n.mountBusy(root) {
		t.Fatal("the awake-instance check sees a download; this test no longer shows the difference")
	}
	if !n.storage.inUse(1, root) {
		t.Fatal("a download in flight does not make its storage in use")
	}
}

// After a hung mount was forced off and the server would not mount again,
// the storage's directory is a plain directory on the node's own disk. No
// task may write a model there: a storage that is not ready gives no root to
// a write, a peer transfer or a cache fill, whatever kept it from being
// ready (failed after the forced unmount, still unresponsive, not checked).
func TestStorageThatIsNotReadyGivesWritersNoRoot(t *testing.T) {
	n := newNodeRuntime(slog.New(slog.DiscardHandler), nil, filepath.Join(t.TempDir(), "models"), "none", metricsOnlyProber{}, nil)
	root := filepath.Join(t.TempDir(), "nfs")
	task := proto.StorageTask{ID: 7, Name: "share", Type: proto.StorageNFS, Path: root, Export: "nas:/m"}
	n.storage.mu.Lock()
	n.storage.tasks[task.ID] = task
	n.storage.mu.Unlock()
	set := func(st *storagemount.Status) {
		n.storage.mu.Lock()
		defer n.storage.mu.Unlock()
		if st == nil {
			delete(n.storage.statuses, task.ID)
			return
		}
		n.storage.statuses[task.ID] = *st
	}
	for name, st := range map[string]*storagemount.Status{
		"remount failed":     {ID: 7, State: storagemount.StateFailed, Reason: storagemount.ReasonUnreachable},
		"still unresponsive": {ID: 7, State: storagemount.StateUnresponsive, Reason: storagemount.ReasonStuckWillRemount},
		"mounting":           {ID: 7, State: storagemount.StateMounting},
		"not checked yet":    nil,
	} {
		set(st)
		if _, ready, err := n.storageRoot(proto.ModelFileTask{StorageID: 7}); ready || err != nil {
			t.Fatalf("%s: a write got the storage's directory (ready=%v err=%v)", name, ready, err)
		}
		if _, err := n.rpcModelSyncListen([]byte(`{"key":"k","transfer_id":"t","storage_id":7,"target_path":"org/m","token":"secret"}`)); err == nil || !strings.Contains(err.Error(), "not ready") {
			t.Fatalf("%s: a peer transfer into the storage's directory was answered %v", name, err)
		}
	}
	set(&storagemount.Status{ID: 7, State: storagemount.StateReady, Writable: true})
	if got, ready, err := n.storageRoot(proto.ModelFileTask{StorageID: 7}); !ready || err != nil || got != root {
		t.Fatalf("a ready storage: root=%q ready=%v err=%v", got, ready, err)
	}
}
