//go:build linux

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// shortenStorageTimeouts sets every limit of r's calls into a share to d. The
// limits belong to the runtime, so another test's runtime still running in the
// background never sees them change.
func shortenStorageTimeouts(r *NodeRuntime, d time.Duration) {
	r.storage.timeouts = storageTimeouts{marker: d, space: d, heartbeat: d, sweep: d, peers: d, remove: d, scanGrace: d}
}

// A completion marker that cannot be read is what a hung NFS mount looks like
// to the agent: the read blocks in the kernel and cannot be interrupted. A
// named pipe blocks the same way, until a writer shows up.
func TestHungMarkerReadDoesNotHoldTheDownloadLockOrPileUp(t *testing.T) {
	r := newStorageTestRuntime(t)
	shortenStorageTimeouts(r.NodeRuntime, 600*time.Millisecond)
	root := t.TempDir()
	r.applyStorages([]proto.StorageTask{{ID: 1, Name: "default", Type: proto.StorageLocal, Path: root}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	hung := filepath.Join(root, "m")
	if err := os.MkdirAll(hung, 0o755); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(hung, modelCompleteMarker)
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	task := proto.ModelFileTask{Key: "f1", StorageID: 1, TargetPath: "m", DesiredState: "present", Acquisition: proto.ModelAcquireCopy,
		Source: "local", SourceID: "m"} // a copy without a source: it fails fast once the marker read is out of the way
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.reconcileModelFile(context.Background(), task)
	}()

	// While the read hangs, everything that takes dlMu still runs.
	time.Sleep(150 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("setup: the marker read did not block")
	default:
	}
	if !r.dlMu.TryLock() {
		t.Fatal("dlMu is held while the marker is read from the storage")
	}
	r.dlMu.Unlock()
	other := filepath.Join(root, "other")
	writeModel(t, other, "w")
	if _, err := r.rpcModelEvict(context.Background(), mustRaw(t, map[string]string{"path": other})); err != nil {
		t.Fatalf("an eviction was held up by the hung read: %v", err)
	}

	// The call gives up at its limit; the task waits for the next storage check.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never gave up on the hung read")
	}
	if got := r.waitingKeys(); len(got) != 1 || got[0] != "f1" {
		t.Fatalf("parked = %v, want the task to wait", got)
	}
	if n := r.fileStatusCount("f1"); n != 0 {
		t.Fatalf("a task whose storage did not answer reported %d statuses", n)
	}

	// Retrying while the read is still stuck starts nothing new and returns at once.
	before := runtime.NumGoroutine()
	begin := time.Now()
	for i := 0; i < 40; i++ {
		r.reconcileModelFile(context.Background(), task)
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Fatalf("40 retries on a hung storage took %s", d)
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d across retries of a hung call", before, after)
	}

	// Once the storage answers, the task runs.
	w, err := os.OpenFile(pipe, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	time.Sleep(100 * time.Millisecond) // the stuck read returns and frees its slot
	if err := os.Remove(pipe); err != nil {
		t.Fatal(err)
	}
	r.reconcileModelFile(context.Background(), task)
	if got := r.waitFile(t, "f1", proto.MFFailed); !strings.Contains(got.Message, "source") {
		t.Fatalf("after the storage answered: %+v", got)
	}
}

func TestCallsIntoAHungShareStartNothingNew(t *testing.T) {
	r := newStorageTestRuntime(t)
	shortenStorageTimeouts(r.NodeRuntime, 100*time.Millisecond)
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	release := make(chan struct{})
	stuck := func(op string) {
		t.Helper()
		if err := r.storageDo(2, op, 20*time.Millisecond, func() error { <-release; return nil }); !errors.Is(err, storagemount.ErrUnresponsive) {
			t.Fatalf("setup %s: %v", op, err)
		}
	}
	target := filepath.Join(shared, "m")
	for _, op := range []string{"heartbeat", "scan", "peers", "space/" + target} {
		stuck(op)
	}
	before := runtime.NumGoroutine()
	hb := proto.StorageHeartbeat{ControlPlaneID: "cp-a", ControlPlaneName: "team-a", Version: "v1", IntervalSeconds: 60}
	begin := time.Now()
	for i := 0; i < 30; i++ {
		r.writeHeartbeat(2, shared, hb)
		if _, err := r.rpcStorageScan(context.Background(), mustRaw(t, proto.StorageScanParams{StorageID: 2})); !errors.Is(err, storagemount.ErrUnresponsive) {
			t.Fatalf("scan of a hung share = %v", err)
		}
		peers := mustRaw(t, proto.StoragePeersParams{StorageID: 2, ControlPlaneID: "cp-a", FreshSeconds: 300})
		if _, err := r.rpcStoragePeers(peers); !errors.Is(err, storagemount.ErrUnresponsive) {
			t.Fatalf("peers of a hung share = %v", err)
		}
		if err := r.checkDownloadSpace(2, target, 1); err == nil || !strings.Contains(err.Error(), "did not answer") {
			t.Fatalf("space check on a hung share = %v", err)
		}
	}
	if d := time.Since(begin); d > time.Second {
		t.Fatalf("calls on a hung share took %s", d)
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d across calls to a hung share", before, after)
	}
	if _, err := os.Stat(filepath.Join(shared, storagemount.MetaDirName, storagemount.ControlPlanesDirName, "cp-a.json")); err == nil {
		t.Fatal("a heartbeat was written while its slot was stuck")
	}

	close(release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.writeHeartbeat(2, shared, hb)
		if _, err := os.Stat(filepath.Join(shared, storagemount.MetaDirName, storagemount.ControlPlanesDirName, "cp-a.json")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat never recovered after the share answered")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
