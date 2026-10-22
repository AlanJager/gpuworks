package store

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

type storageFixture struct {
	st     *Store
	local  *Storage
	nfs    *Storage
	cache  *Storage
	n1, n2 *Node
	model  *Model
}

func newStorageFixture(t *testing.T) *storageFixture {
	t.Helper()
	b := bus.New()
	st, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &storageFixture{st: st}
	if f.local, err = st.StorageByID(st.DefaultLocalStorageID()); err != nil {
		t.Fatal(err)
	}
	if f.nfs, err = st.CreateStorage(&Storage{Name: "model-archive", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/model-archive", Export: "nfs.example.internal:/export/models"}, nil); err != nil {
		t.Fatal(err)
	}
	if f.cache, err = st.CreateStorage(&Storage{Name: "archive-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/archive-cache", Export: "nfs.example.internal:/export/cache", CacheMaxBytes: 100}, nil); err != nil {
		t.Fatal(err)
	}
	if f.n1, err = st.RegisterNode("n1", HashToken("t1"), "n1"); err != nil {
		t.Fatal(err)
	}
	if f.n2, err = st.RegisterNode("n2", HashToken("t2"), "n2"); err != nil {
		t.Fatal(err)
	}
	if f.model, err = st.UpsertModel(&Model{Source: "modelscope", SourceID: "Qwen/Qwen3-8B", Name: "Qwen3-8B", WeightBytes: 15}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *storageFixture) report(t *testing.T, node *Node, statuses ...proto.StorageStatus) {
	t.Helper()
	if err := f.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: statuses}); err != nil {
		t.Fatal(err)
	}
}

func TestModelLocationPerStorageType(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	// Local storage: the node's own row decides.
	loc, err := st.ModelLocation(f.model.ID, f.local.ID, f.n1.ID)
	if err != nil || loc.Ready || loc.Reason != LocationNotOnNode {
		t.Fatalf("local absent = %+v err=%v", loc, err)
	}
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/data/models/x", State: proto.MFPresent, SizeBytes: 15}); err != nil {
		t.Fatal(err)
	}
	if loc, _ = st.ModelLocation(f.model.ID, f.local.ID, f.n1.ID); !loc.Ready || loc.HostPath != "/data/models/x" {
		t.Fatalf("local present = %+v", loc)
	}
	// NFS: one shared row; each node reads it through its own mount point.
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n1.ID); loc.Ready || loc.Reason != LocationModelMissingInStore {
		t.Fatalf("nfs without the model = %+v", loc)
	}
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15, Acquisition: proto.ModelAcquireImported}); err != nil {
		t.Fatal(err)
	}
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n1.ID); loc.Ready || loc.Reason != LocationStorageUnavailable {
		t.Fatalf("nfs not mounted = %+v", loc)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	f.report(t, f.n2, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/srv/archive"})
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n1.ID); !loc.Ready || loc.HostPath != "/mnt/gpuworks/model-archive/Qwen/Qwen3-8B" {
		t.Fatalf("nfs on n1 = %+v", loc)
	}
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n2.ID); !loc.Ready || loc.HostPath != "/srv/archive/Qwen/Qwen3-8B" {
		t.Fatalf("nfs on n2 (own mount point) = %+v", loc)
	}
	f.report(t, f.n2, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateUnresponsive, Path: "/srv/archive"})
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n2.ID); loc.Ready || loc.Reason != LocationStorageUnavailable {
		t.Fatalf("nfs unresponsive on n2 = %+v", loc)
	}
	if err := st.SetModelFileState(loc.Shared.ID, proto.MFMissing, "gone"); err != nil {
		t.Fatal(err)
	}
	if loc, _ = st.ModelLocation(f.model.ID, f.nfs.ID, f.n1.ID); loc.Ready || loc.Reason != LocationModelMissingInStore {
		t.Fatalf("nfs missing = %+v", loc)
	}
	// nfs_cache: the node reads its own cache copy, once there is one.
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 15}); err != nil {
		t.Fatal(err)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true},
		proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/archive-cache", CachePath: "/var/lib/gpuworks/cache/archive-cache"})
	if loc, _ = st.ModelLocation(f.model.ID, f.cache.ID, f.n1.ID); loc.Ready || loc.Reason != LocationNotCached || loc.SizeBytes != 15 {
		t.Fatalf("cache not filled = %+v", loc)
	}
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID, Path: "/var/lib/gpuworks/cache/archive-cache/m", State: proto.MFPresent, SizeBytes: 15}); err != nil {
		t.Fatal(err)
	}
	if loc, _ = st.ModelLocation(f.model.ID, f.cache.ID, f.n1.ID); !loc.Ready || loc.HostPath != "/var/lib/gpuworks/cache/archive-cache/m" {
		t.Fatalf("cache filled = %+v", loc)
	}
	// A storage limited to n1 does not apply to n2.
	limited, err := st.CreateStorage(&Storage{Name: "nvme", Type: proto.StorageLocal, Enabled: true, Path: "/nvme"}, []StorageNode{{NodeID: f.n1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if loc, _ = st.ModelLocation(f.model.ID, limited.ID, f.n2.ID); loc.Reason != LocationStorageNotOnNode {
		t.Fatalf("limited storage on n2 = %+v", loc)
	}
	locs, err := st.ModelLocations(f.model.ID, f.n1.ID)
	if err != nil || len(locs) != 4 || locs[0].Storage.Type != proto.StorageLocal || locs[1].Storage.Type != proto.StorageLocal {
		t.Fatalf("locations on n1 = %d err=%v, want local storages first", len(locs), err)
	}
	if ready, _ := st.ReadyLocation(f.model.ID, f.n1.ID); ready == nil || ready.Storage.ID != f.local.ID {
		t.Fatalf("ready location = %+v", ready)
	}
}

func TestApplyStorageStatusWritesOnlyChanges(t *testing.T) {
	f := newStorageFixture(t)
	ready := proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true, TotalBytes: 1000, AvailableBytes: 500}
	f.report(t, f.n1, ready)
	first, err := f.st.StorageNodeStateFor(f.nfs.ID, f.n1.ID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	// The same values, capacity moving under 1% of the total: no write.
	same := ready
	same.AvailableBytes = 495
	for i := 0; i < 20; i++ {
		f.report(t, f.n1, same)
	}
	again, _ := f.st.StorageNodeStateFor(f.nfs.ID, f.n1.ID)
	if !again.UpdatedAt.Equal(first.UpdatedAt) {
		t.Fatalf("unchanged report rewrote the row: %v -> %v", first.UpdatedAt, again.UpdatedAt)
	}
	storage, _ := f.st.StorageByID(f.nfs.ID)
	if storage.TotalBytes != 1000 || storage.AvailableBytes != 500 {
		t.Fatalf("capacity = %d/%d, want the first report kept", storage.AvailableBytes, storage.TotalBytes)
	}
	moved := ready
	moved.AvailableBytes = 400
	f.report(t, f.n1, moved)
	if storage, _ = f.st.StorageByID(f.nfs.ID); storage.AvailableBytes != 400 {
		t.Fatalf("a 10%% move was not recorded: %d", storage.AvailableBytes)
	}
	failed := ready
	failed.State, failed.Reason = proto.StorageStateFailed, "unreachable"
	f.report(t, f.n1, failed)
	if row, _ := f.st.StorageNodeStateFor(f.nfs.ID, f.n1.ID); row.State != proto.StorageStateFailed || row.Reason != "unreachable" {
		t.Fatalf("state change not written: %+v", row)
	}
	// A storage missing from the report is gone from the node.
	f.report(t, f.n1)
	if _, err := f.st.StorageNodeStateFor(f.nfs.ID, f.n1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("state after the storage left the report: err=%v", err)
	}
	// Reports for unknown storages are ignored.
	f.report(t, f.n1, proto.StorageStatus{ID: 999, State: proto.StorageStateReady})
}

func TestStorageAcceptsWritesAndHeartbeatNode(t *testing.T) {
	f := newStorageFixture(t)
	if err := f.st.StorageAcceptsWrites(f.local); err != nil {
		t.Fatalf("enabled local storage refused: %v", err)
	}
	if err := f.st.StorageAcceptsWrites(f.nfs); !errors.Is(err, ErrStorageNotWritable) {
		t.Fatalf("nfs without a writable node = %v", err)
	}
	f.report(t, f.n2, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/m", Writable: true})
	if err := f.st.StorageAcceptsWrites(f.nfs); err != nil {
		t.Fatalf("nfs with a writable node refused: %v", err)
	}
	disabled := false
	if _, err := f.st.UpdateStorage(f.nfs.ID, StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	nfs, _ := f.st.StorageByID(f.nfs.ID)
	if err := f.st.StorageAcceptsWrites(nfs); !errors.Is(err, ErrStorageDisabled) {
		t.Fatalf("disabled storage = %v", err)
	}
	// Heartbeat duty goes to the lowest online node that can write.
	if id, _ := f.st.HeartbeatNode(nfs); id != 0 {
		t.Fatalf("heartbeat node with every node offline = %d", id)
	}
	for _, n := range []*Node{f.n1, f.n2} {
		if err := f.st.DB().Model(&Node{}).Where("id = ?", n.ID).Update("state", NodeStateOnline).Error; err != nil {
			t.Fatal(err)
		}
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/m", Writable: false})
	if id, _ := f.st.HeartbeatNode(nfs); id != f.n2.ID {
		t.Fatalf("heartbeat node = %d, want the writable node %d", id, f.n2.ID)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/m", Writable: true})
	if id, _ := f.st.HeartbeatNode(nfs); id != f.n1.ID {
		t.Fatalf("heartbeat node = %d, want the lowest writable node %d", id, f.n1.ID)
	}
	tasks, err := f.st.StorageTasksForNode(f.n1)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		// A disabled storage writes no heartbeat.
		if task.Heartbeat != nil {
			t.Fatalf("heartbeat on a disabled storage: %+v", task)
		}
	}
}

func TestStorageInvariants(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	if _, err := st.CreateStorage(&Storage{Name: "dup", Type: proto.StorageNFS, Export: "NFS.example.internal:/export/models/", Path: "/mnt/x"}, nil); !errors.Is(err, ErrStorageExportTaken) {
		t.Fatalf("second registration of an export = %v", err)
	}
	makeDefault := true
	if _, err := st.UpdateStorage(f.nfs.ID, StoragePatch{IsDefault: &makeDefault}); err != nil {
		t.Fatal(err)
	}
	var defaults int64
	if err := st.DB().Model(&Storage{}).Where("is_default = ?", true).Count(&defaults).Error; err != nil || defaults != 1 {
		t.Fatalf("default storages = %d err=%v", defaults, err)
	}
	if def, _ := st.DefaultStorage(); def.ID != f.nfs.ID {
		t.Fatalf("default = %+v", def)
	}
	// A reference without a storage still means the default local storage.
	if st.DefaultLocalStorageID() != f.local.ID {
		t.Fatalf("default local storage = %d", st.DefaultLocalStorageID())
	}
	dep, err := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", ModelID: f.model.ID, NodeID: f.n1.ID})
	if err != nil {
		t.Fatal(err)
	}
	// A deployment that names no storage keeps none: where a node saves a
	// model it must fetch follows the default order at that time.
	if dep.SaveToStorageID != 0 {
		t.Fatalf("deployment without a storage saves into %d, want 0", dep.SaveToStorageID)
	}
	// A storage a deployment is limited to by a node requirement cannot go.
	pinned, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "p", ServedName: "p", ModelID: f.model.ID, NodeID: f.n1.ID},
		nodelabel.Requirements{nodelabel.KeyStorage: {strconv.FormatUint(uint64(f.local.ID), 10)}})
	if err != nil {
		t.Fatal(err)
	}
	var inUse *StorageInUseError
	if err := st.DeleteStorage(f.local.ID); !errors.As(err, &inUse) || len(inUse.DeploymentIDs) != 1 || inUse.DeploymentIDs[0] != pinned.ID {
		t.Fatalf("delete a storage a deployment is limited to = %v", err)
	}
	if ids, err := st.DeploymentsUsingStorage(f.local.ID); err != nil || len(ids) != 1 || ids[0] != pinned.ID {
		t.Fatalf("deployments using the storage = %v err=%v", ids, err)
	}
	// Moving a storage's directory under a live instance that reads one of
	// its files is refused.
	file, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/opt/gpuworks/models/q", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	reader := &Instance{UUID: "i-1", DeploymentID: dep.ID, NodeID: f.n1.ID, State: proto.InstRunning}
	if err := st.CreateInstances([]*Instance{reader}, [][]InstanceModelFile{{{ModelID: f.model.ID, ModelFileID: file.ID, HostPath: file.Path}}}); err != nil {
		t.Fatal(err)
	}
	moved := "/elsewhere"
	if _, err := st.UpdateStorage(f.local.ID, StoragePatch{Path: &moved}); !errors.Is(err, ErrStorageInUse) {
		t.Fatalf("move under a live instance = %v", err)
	}
	// Renaming is not a move.
	name := "local-main"
	if _, err := st.UpdateStorage(f.local.ID, StoragePatch{Name: &name}); err != nil {
		t.Fatalf("rename = %v", err)
	}
	// Removing a storage nothing reads removes its index rows, not files. A
	// deployment that only saved into it saves by the default order after.
	saver, err := st.CreateDeployment(&Deployment{Name: "s", ServedName: "s", ModelID: f.model.ID, NodeID: f.n2.ID, SaveToStorageID: f.cache.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteStorage(f.cache.ID); err != nil {
		t.Fatal(err)
	}
	if rows, _ := st.ModelFilesInStorage(f.cache.ID); len(rows) != 0 {
		t.Fatalf("rows left after removing the storage: %+v", rows)
	}
	if after, _ := st.GetDeployment(saver.ID); after.SaveToStorageID != 0 {
		t.Fatalf("deployment still saves into the removed storage %d", after.SaveToStorageID)
	}
}

func TestModelFileTasksForNodeIncludeWriterRowsWithStaging(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true})
	row, err := st.PrepareModelDownload(FileRef{ModelID: f.model.ID, StorageID: f.nfs.ID}, SharedModelRelPath(f.model))
	if err != nil {
		t.Fatal(err)
	}
	if tasks, _ := st.ModelFileTasksForNode(f.n1.ID); len(tasks) != 0 {
		t.Fatalf("a shared row without a writer was sent: %+v", tasks)
	}
	if err := st.SetModelFileWriter(row.ID, f.n1.ID); err != nil {
		t.Fatal(err)
	}
	tasks, err := st.ModelFileTasksForNode(f.n1.ID)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("writer tasks = %+v err=%v", tasks, err)
	}
	task := tasks[0]
	identity := st.ControlPlaneIdentity()
	if task.Key != ModelFileKey(row.ID) || task.StorageID != f.nfs.ID || task.TargetPath != "Qwen/Qwen3-8B" ||
		task.Staging != ".gpuworks/incoming/"+identity.ID+"/Qwen/Qwen3-8B" || task.Cache {
		t.Fatalf("writer task = %+v", task)
	}
	if tasks, _ := st.ModelFileTasksForNode(f.n2.ID); len(tasks) != 0 {
		t.Fatalf("a node that is not the writer got the task: %+v", tasks)
	}
	// The writer's report is accepted; another node's is not. The shared row
	// keeps its storage-relative path.
	if err := st.UpdateModelFileStatus(f.n2.ID, proto.ModelFileStatus{Key: task.Key, State: proto.MFPresent}); err == nil {
		t.Fatal("a node that is not the writer updated the shared row")
	}
	if err := st.UpdateModelFileStatus(f.n1.ID, proto.ModelFileStatus{Key: task.Key, State: proto.MFPresent, Path: "/mnt/gpuworks/model-archive/Qwen/Qwen3-8B", BytesTotal: 15, BytesDone: 15}); err != nil {
		t.Fatal(err)
	}
	done, _ := st.ModelFileByID(row.ID)
	if done.State != proto.MFPresent || done.Path != "Qwen/Qwen3-8B" {
		t.Fatalf("shared row after completion = %+v", done)
	}
	if tasks, _ := st.ModelFileTasksForNode(f.n1.ID); len(tasks) != 0 {
		t.Fatalf("a complete shared row is still a task: %+v", tasks)
	}
	// A node cache copy reads from the node's own mount point.
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true},
		proto.StorageStatus{ID: f.cache.ID, State: proto.StorageStateReady, Path: "/mnt/c"})
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareModelCopy(FileRef{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID}, ModelArtifactPath(f.model)); err != nil {
		t.Fatal(err)
	}
	tasks, _ = st.ModelFileTasksForNode(f.n1.ID)
	if len(tasks) != 1 || !tasks[0].Cache || tasks[0].SourcePath != "/mnt/c/Qwen/Qwen3-8B" || tasks[0].Acquisition != proto.ModelAcquireCopy {
		t.Fatalf("cache task = %+v", tasks)
	}
	// Legacy keys from agents that predate storages still resolve.
	local, err := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, NodeID: f.n2.ID, Path: "p", State: proto.MFPending})
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Join([]string{uintKey(f.model.ID), uintKey(f.n2.ID)}, "-")
	if got, err := st.ResolveModelFileKey(legacy); err != nil || got.ID != local.ID {
		t.Fatalf("legacy key = %+v err=%v", got, err)
	}
}

func TestNodeCacheEntriesDeriveUseFromDeploymentsAndInstances(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	other, err := st.UpsertModel(&Model{Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Name: "small"})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []*Model{f.model, other} {
		if _, err := st.UpsertModelFile(&ModelFile{ModelID: m.ID, StorageID: f.cache.ID, Path: m.SourceID, State: proto.MFPresent}); err != nil {
			t.Fatal(err)
		}
	}
	used := time.Now().Add(-time.Hour)
	a, _ := st.UpsertModelFile(&ModelFile{ModelID: f.model.ID, StorageID: f.cache.ID, NodeID: f.n1.ID, Path: f.cache.CachePath + "/a", State: proto.MFPresent, SizeBytes: 15, LastUsedAt: &used})
	b, _ := st.UpsertModelFile(&ModelFile{ModelID: other.ID, StorageID: f.cache.ID, NodeID: f.n1.ID, Path: f.cache.CachePath + "/b", State: proto.MFDownloading, SizeBytes: 2})
	if _, err := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", ModelID: f.model.ID, NodeID: f.n1.ID, SaveToStorageID: f.cache.ID}); err != nil {
		t.Fatal(err)
	}
	entries, err := st.NodeCacheEntries(f.cache.ID, f.n1.ID)
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v err=%v", entries, err)
	}
	byID := map[uint]int{entries[0].ID: 0, entries[1].ID: 1}
	ea, eb := entries[byID[a.ID]], entries[byID[b.ID]]
	if !ea.Referenced || !ea.Backed || ea.Transferring || ea.LastUsed == nil || ea.Evictable() {
		t.Fatalf("entry a = %+v, want referenced by the active deployment", ea)
	}
	if eb.Referenced || !eb.Transferring || eb.Evictable() {
		t.Fatalf("entry b = %+v, want transferring", eb)
	}
}
