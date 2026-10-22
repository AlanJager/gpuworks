package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/agent/storagemount"
	"gpuworks.ai/gpuworks/internal/proto"
)

// storageTestRuntime is a node runtime without a container runtime, whose
// emitted messages are recorded.
type storageTestRuntime struct {
	*NodeRuntime
	fs       *shareFS
	mu       sync.Mutex
	storages []proto.StorageStatusReport
	files    []proto.ModelFileStatus
}

// shareFS stands in for the node's statfs: a directory counts as a mounted
// network share unless a test says it sits on the local disk, as a share's
// mount point does after the mount dropped.
type shareFS struct {
	mu       sync.Mutex
	local    map[string]bool
	infoPath string // the mountinfo the node reads
	mounts   []string
}

// mountNFS makes path an NFS mount point in the node's mountinfo.
func (f *shareFS) mountNFS(t *testing.T, path string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = append(f.mounts, fmt.Sprintf("%d 22 0:%d / %s rw,noatime shared:%d - nfs4 nas:/export%d rw,vers=4.2",
		100+len(f.mounts), 50+len(f.mounts), path, 200+len(f.mounts), len(f.mounts)))
	if err := os.WriteFile(f.infoPath, []byte("22 1 8:1 / / rw - ext4 /dev/sda1 rw\n"+strings.Join(f.mounts, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *shareFS) onLocalDisk(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.local[filepath.Clean(path)] = true
}

func (f *shareFS) statfs(path string) (storagemount.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	magic := uint64(0x6969) // NFS_SUPER_MAGIC
	for dir := filepath.Clean(path); ; dir = filepath.Dir(dir) {
		if f.local[dir] {
			magic = 0xEF53 // ext4
			break
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return storagemount.Usage{TotalBytes: 100 << 30, AvailableBytes: 40 << 30, FSType: magic}, nil
}

func newStorageTestRuntime(t *testing.T) *storageTestRuntime {
	t.Helper()
	return newStorageTestRuntimeLog(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newStorageTestRuntimeLog(t *testing.T, log *slog.Logger) *storageTestRuntime {
	t.Helper()
	models := filepath.Join(t.TempDir(), "models")
	n := newNodeRuntime(log, nil, models, "none", metricsOnlyProber{}, nil)
	r := &storageTestRuntime{NodeRuntime: n, fs: &shareFS{local: map[string]bool{}, infoPath: filepath.Join(t.TempDir(), "mountinfo")}}
	r.fs.mountNFS(t, "/nonexistent-share")
	n.storage.manager = storagemount.New(storagemount.Options{
		Statfs: r.fs.statfs, MountinfoPath: r.fs.infoPath, ProbeDir: filepath.Join(t.TempDir(), "probe"), Logger: log,
	})
	n.Bind(func(kind string, value any) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		switch kind {
		case proto.TStatusStorage:
			r.storages = append(r.storages, value.(proto.StorageStatusReport))
		case proto.TStatusModelFile:
			r.files = append(r.files, value.(proto.ModelFileStatus))
		}
		return nil
	})
	return r
}

func (r *storageTestRuntime) lastFile(t *testing.T, key string) proto.ModelFileStatus {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.files) - 1; i >= 0; i-- {
		if r.files[i].Key == key {
			return r.files[i]
		}
	}
	t.Fatalf("no status for %s", key)
	return proto.ModelFileStatus{}
}

func (r *storageTestRuntime) waitFile(t *testing.T, key string, states ...string) proto.ModelFileStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for i := len(r.files) - 1; i >= 0; i-- {
			if r.files[i].Key != key {
				continue
			}
			for _, state := range states {
				if r.files[i].State == state {
					st := r.files[i]
					r.mu.Unlock()
					return st
				}
			}
			break
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never reached %v; statuses=%+v", key, states, r.files)
	return proto.ModelFileStatus{}
}

func writeModel(t *testing.T, dir string, weights string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".msc-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.json": `{"architectures":["Qwen3ForCausalLM"]}`, "model.safetensors": weights, "tokenizer.json": "{}", ".msc-dir/meta": "skip me"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoragesBecomeReadyAndAreReportedOnlyWhenTheyChange(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	r.fs.mountNFS(t, shared) // the existing share of storage 3
	r.applyStorages([]proto.StorageTask{
		{ID: 1, Name: "default", Type: proto.StorageLocal, Path: r.ModelsRoot()},
		{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared},
		{ID: 3, Name: "nfs-cached", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache},
	})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	if len(r.storages) != 1 || len(r.storages[0].Storages) != 3 {
		t.Fatalf("reports = %+v", r.storages)
	}
	for _, st := range r.storages[0].Storages {
		if st.State != proto.StorageStateReady {
			t.Fatalf("storage %d = %+v", st.ID, st)
		}
		if st.ID == 1 && st.Path != r.ModelsRoot() {
			t.Fatalf("default local storage path = %q, want the models root", st.Path)
		}
		if st.ID == 3 && st.CachePath != cache {
			t.Fatalf("cache path = %q", st.CachePath)
		}
	}
	r.checkStorages(context.Background(), false)
	if len(r.storages) != 1 {
		t.Fatalf("an unchanged check was reported again: %d reports", len(r.storages))
	}
	usage := r.storagePathUsage()
	kinds := map[string]bool{}
	for _, u := range usage {
		kinds[u.Kind] = true
	}
	if !kinds["storage"] || !kinds["storage_cache"] {
		t.Fatalf("path usage = %+v", usage)
	}
	if err := os.RemoveAll(shared); err != nil {
		t.Fatal(err)
	}
	r.checkStorages(context.Background(), false)
	if len(r.storages) != 2 {
		t.Fatalf("a storage that went away was not reported: %d reports", len(r.storages))
	}
	roots := strings.Join(r.storageRoots(), ",")
	if !strings.Contains(roots, cache) || strings.Contains(roots, shared) {
		t.Fatalf("writable roots = %s, want local roots and caches only", roots)
	}
}

func TestStorageReportChangedIgnoresSmallCapacityDrift(t *testing.T) {
	base := []proto.StorageStatus{{ID: 1, State: "ready", TotalBytes: 1000, AvailableBytes: 500}}
	small := []proto.StorageStatus{{ID: 1, State: "ready", TotalBytes: 1000, AvailableBytes: 495}}
	big := []proto.StorageStatus{{ID: 1, State: "ready", TotalBytes: 1000, AvailableBytes: 480}}
	if storageReportChanged(base, small) {
		t.Fatal("0.5% drift reported")
	}
	if !storageReportChanged(base, big) {
		t.Fatal("2% drift not reported")
	}
	if !storageReportChanged(base, []proto.StorageStatus{{ID: 1, State: "failed", TotalBytes: 1000, AvailableBytes: 500}}) {
		t.Fatal("state change not reported")
	}
}

func TestModelFileTaskWaitsForItsStorage(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	source := filepath.Join(t.TempDir(), "source")
	writeModel(t, source, "weights")
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	task := proto.ModelFileTask{Key: "f7", StorageID: 2, RelPath: "Qwen/Qwen3-8B", DesiredState: "present",
		Acquisition: proto.ModelAcquireCopy, SourcePath: source, Staging: ".gpuworks/incoming/cp/Qwen/Qwen3-8B",
		Source: "local", SourceID: "Qwen/Qwen3-8B"}
	// The storage was never checked: the task waits rather than writing into
	// what may be a bare mount point.
	r.reconcileModelFile(context.Background(), task)
	if len(r.files) != 0 {
		t.Fatalf("task ran before its storage was ready: %+v", r.files)
	}
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	got := r.waitFile(t, "f7", proto.MFPresent, proto.MFFailed)
	if got.State != proto.MFPresent {
		t.Fatalf("copy into the shared storage = %+v", got)
	}
	final := filepath.Join(shared, "Qwen/Qwen3-8B")
	if _, err := modelscan.ValidateModelDir(final); err != nil {
		t.Fatalf("final directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(final, ".msc-dir")); !os.IsNotExist(err) {
		t.Fatal("tool metadata directory was copied")
	}
	if _, err := os.Stat(filepath.Join(shared, ".gpuworks/incoming/cp/Qwen")); !os.IsNotExist(err) {
		t.Fatal("staging directories left behind")
	}
	if _, err := os.Stat(filepath.Join(shared, ".gpuworks/incoming/cp")); err != nil {
		t.Fatalf("the control plane's incoming directory itself was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(final, modelCompleteMarker)); err != nil {
		t.Fatalf("completion marker: %v", err)
	}
	// A task for a storage the node does not have fails with a reason.
	r.reconcileModelFile(context.Background(), proto.ModelFileTask{Key: "f8", StorageID: 99, TargetPath: "x", DesiredState: "present"})
	if got := r.lastFile(t, "f8"); got.State != proto.MFFailed || !strings.Contains(got.Message, "not configured") {
		t.Fatalf("unknown storage task = %+v", got)
	}
}

func TestHeartbeatNodeWritesHeartbeatAndSweepsTrash(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	abandoned := storagemount.TrashPath(shared, "Qwen/Qwen3-8B", time.Now().Add(-2*trashSweepAge))
	inFlight := storagemount.TrashPath(shared, "Qwen/Qwen3-4B", time.Now())
	for _, dir := range []string{abandoned, inFlight} {
		writeModel(t, dir, "weights")
	}
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "shared", Type: proto.StorageSharedDir, Path: shared,
		Heartbeat: &proto.StorageHeartbeat{ControlPlaneID: "cp-a", ControlPlaneName: "team-a", IntervalSeconds: 60}}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	r.refreshHeartbeats()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, hbErr := os.Stat(filepath.Join(shared, ".gpuworks", "control-planes", "cp-a.json"))
		_, trashErr := os.Stat(abandoned)
		if hbErr == nil && os.IsNotExist(trashErr) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat err=%v, abandoned trash err=%v", hbErr, trashErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("a removal still in flight was swept: %v", err)
	}
}

func TestStorageRPCs(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	writeModel(t, filepath.Join(shared, "Qwen", "Qwen3-8B"), "weights")
	if err := os.MkdirAll(filepath.Join(shared, "deepseek-ai", "Empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "archive", Type: proto.StorageSharedDir, Path: shared}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)

	raw, _ := json.Marshal(proto.StorageScanParams{StorageID: 2})
	out, err := r.rpcStorageScan(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	scan := out.(proto.StorageScanResult)
	if !scan.Complete || len(scan.Candidates) != 1 || scan.Candidates[0].RelPath != "Qwen/Qwen3-8B" {
		t.Fatalf("scan = %+v", scan)
	}

	peersRaw, _ := json.Marshal(proto.StoragePeersParams{StorageID: 2, ControlPlaneID: "cp-a", ControlPlaneName: "team-a", FreshSeconds: 300})
	out, err = r.rpcStoragePeers(peersRaw)
	if err != nil {
		t.Fatal(err)
	}
	if peers := out.(proto.StoragePeersResult).Peers; len(peers) != 0 {
		t.Fatalf("peers of the only control plane = %+v", peers)
	}
	otherRaw, _ := json.Marshal(proto.StoragePeersParams{StorageID: 2, ControlPlaneID: "cp-b", ControlPlaneName: "team-b", FreshSeconds: 300})
	out, err = r.rpcStoragePeers(otherRaw)
	if err != nil {
		t.Fatal(err)
	}
	if peers := out.(proto.StoragePeersResult).Peers; len(peers) != 1 || peers[0].ID != "cp-a" || !peers[0].Fresh {
		t.Fatalf("peers seen by a second control plane = %+v", peers)
	}

	for _, bad := range []string{"../escape", ".gpuworks/control-planes", "/abs", "."} {
		badRaw, _ := json.Marshal(proto.StorageRemoveParams{StorageID: 2, RelPath: bad})
		if _, err := r.rpcStorageRemove(badRaw); err == nil {
			t.Fatalf("remove %q accepted", bad)
		}
	}
	removeRaw, _ := json.Marshal(proto.StorageRemoveParams{StorageID: 2, RelPath: "Qwen/Qwen3-8B"})
	if _, err := r.rpcStorageRemove(removeRaw); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, "Qwen", "Qwen3-8B")); !os.IsNotExist(err) {
		t.Fatal("removed model is still at its path")
	}
	// Scanning or removing on a storage the node does not have is refused.
	missing, _ := json.Marshal(proto.StorageScanParams{StorageID: 9})
	if _, err := r.rpcStorageScan(context.Background(), missing); err == nil {
		t.Fatal("scan of an unknown storage accepted")
	}

	probeRaw, _ := json.Marshal(proto.StorageProbeParams{Type: proto.StorageSharedDir, Path: shared})
	out, err = r.rpcStorageProbe(context.Background(), probeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if probe := out.(proto.StorageProbeResult); !probe.OK || !probe.Readable || !probe.Writable {
		t.Fatalf("probe = %+v", probe)
	}
}

func TestEvictionIsLimitedToLocalStoragesAndCaches(t *testing.T) {
	r := newStorageTestRuntime(t)
	shared := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	r.fs.mountNFS(t, shared)
	r.applyStorages([]proto.StorageTask{{ID: 3, Name: "c", Type: proto.StorageNFSCache, Path: shared, Existing: true, CachePath: cache}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	cached := filepath.Join(cache, "modelscope", "m")
	writeModel(t, cached, "w")
	inShared := filepath.Join(shared, "m")
	writeModel(t, inShared, "w")
	raw, _ := json.Marshal(map[string]string{"path": cached})
	if _, err := r.rpcModelEvict(context.Background(), raw); err != nil {
		t.Fatalf("evict a cache entry: %v", err)
	}
	raw, _ = json.Marshal(map[string]string{"path": inShared})
	if _, err := r.rpcModelEvict(context.Background(), raw); err == nil {
		t.Fatal("evict reached into a shared storage")
	}
}

// The own-directory offer is what keeps a custom-state agent's models where
// that agent put them: the server records the offer as the node's default
// local storage directory before the first desired state is built, so a root
// agent installed outside /opt/gpuworks (a cloud install on a data disk, a
// CI container) is not silently relocated to the global default. Only the
// standard root layout stays with the storages' own directories - there the
// two paths are the same directory.
func TestOwnModelsDir(t *testing.T) {
	cases := []struct {
		name     string
		stateDir string
		euid     int
		want     string
	}{
		{"non-root standard layout offers its directory", "/opt/gpuworks", 1000, "/opt/gpuworks/models"},
		{"non-root custom state offers its directory", "/data", 1000, "/data/models"},
		{"root standard layout keeps the storages' directories", "/opt/gpuworks", 0, ""},
		{"root custom state offers its directory", "/data", 0, "/data/models"},
		{"root custom state with unclean path still matches nothing", "/opt/gpuworks/", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownModelsDir(tc.stateDir, tc.euid); got != tc.want {
				t.Fatalf("ownModelsDir(%q, %d) = %q, want %q", tc.stateDir, tc.euid, got, tc.want)
			}
		})
	}
}
