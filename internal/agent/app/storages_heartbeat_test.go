package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// recordedLogs collects the messages of the records at or above a level.
type recordedLogs struct {
	mu       sync.Mutex
	level    slog.Level
	messages []string
}

func (h *recordedLogs) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *recordedLogs) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}
func (h *recordedLogs) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordedLogs) WithGroup(string) slog.Handler      { return h }

func (h *recordedLogs) count(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			n++
		}
	}
	return n
}

func heartbeatFile(shared string) string {
	return filepath.Join(shared, proto.StorageMetaDir, proto.StorageControlPlanesDir, "cp-a.json")
}

// The heartbeat names the control plane's own version, as the server sent it.
func TestHeartbeatCarriesTheServersVersion(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared,
		Heartbeat: &proto.StorageHeartbeat{ControlPlaneID: "cp-a", ControlPlaneName: "team-a", Version: "v9.8.7", IntervalSeconds: 60}}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	var hb storagemount.Heartbeat
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(heartbeatFile(shared))
		if err == nil && json.Unmarshal(b, &hb) == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no heartbeat written: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if hb.ID != "cp-a" || hb.Name != "team-a" || hb.Version != "v9.8.7" {
		t.Fatalf("heartbeat = %+v; the agent must not stamp its own version", hb)
	}
}

// A heartbeat without an interval is a protocol error: the agent does not
// invent one, skips the heartbeat and says so once.
func TestHeartbeatWithoutAnIntervalIsSkippedAndLoggedOnce(t *testing.T) {
	logs := &recordedLogs{level: slog.LevelError}
	r := newStorageTestRuntimeLog(t, slog.New(logs))
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared,
		Heartbeat: &proto.StorageHeartbeat{ControlPlaneID: "cp-a", ControlPlaneName: "team-a", Version: "v1"}}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	for i := 0; i < 3; i++ {
		r.refreshHeartbeats()
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(heartbeatFile(shared)); err == nil {
		t.Fatal("a heartbeat was written without a stated interval")
	}
	if n := logs.count("heartbeat skipped"); n != 1 {
		t.Fatalf("protocol error logged %d times, want once", n)
	}
	// The server corrects itself: the heartbeat starts, and a later regression is reported again.
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared,
		Heartbeat: &proto.StorageHeartbeat{ControlPlaneID: "cp-a", ControlPlaneName: "team-a", Version: "v1", IntervalSeconds: 60}}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(heartbeatFile(shared)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat after the interval was given")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPeersRequestWithoutFreshnessIsRefused(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	if _, err := r.rpcStoragePeers(mustRaw(t, proto.StoragePeersParams{StorageID: 2, ControlPlaneID: "cp-a", ControlPlaneName: "team-a"})); err == nil ||
		!strings.Contains(err.Error(), "fresh_seconds") {
		t.Fatalf("peers without freshness = %v", err)
	}
	if _, err := os.Stat(heartbeatFile(shared)); err == nil {
		t.Fatal("a refused request still wrote the heartbeat")
	}
	// Removing the heartbeat states no freshness and needs none.
	if _, err := r.rpcStoragePeers(mustRaw(t, proto.StoragePeersParams{StorageID: 2, ControlPlaneID: "cp-a", Remove: true})); err != nil {
		t.Fatalf("remove = %v", err)
	}
}
