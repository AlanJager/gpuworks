package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestModelSyncStatusRejectsStaleTransfersAndRegressions(t *testing.T) {
	st, err := Open("file:store_model_sync_status?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "test/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("sync-node", HashToken("token"), "sync-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "model", State: proto.MFPending, Acquisition: proto.ModelAcquireAuto,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.BeginModelSync(FileRef{ModelID: model.ID, NodeID: node.ID}, 99, "current-transfer", 1000); err != nil {
		t.Fatal(err)
	}
	key := mfKeyFor(t, st, model.ID, node.ID)
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFPresent, TransferID: "stale-transfer", BytesTotal: 5, BytesDone: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFDownloading, BytesTotal: 50, BytesDone: 10,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.ModelFileByModelNode(model.ID, node.ID)
	if got.State != proto.MFSyncing || got.TransferID != "current-transfer" || got.SizeBytes != 1000 || got.Downloaded != 0 {
		t.Fatalf("stale status changed active sync: %+v", got)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFSyncing, TransferID: "current-transfer", Acquisition: proto.ModelAcquirePeer,
		BytesTotal: 1000, BytesDone: 400, TransferBytesDone: 450, RateBytesPerS: 123,
	}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ModelFileByModelNode(model.ID, node.ID)
	if got.Downloaded != 400 || got.TransferredBytes != 450 || got.RateBPS != 123 {
		t.Fatalf("current transfer progress not applied: %+v", got)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFPresent, TransferID: "current-transfer", Acquisition: proto.ModelAcquirePeer,
		BytesTotal: 1000, BytesDone: 1000, Path: "/models/ready", Revision: "abc123",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFPresent, TransferID: "current-transfer", Acquisition: proto.ModelAcquirePeer,
		Path: "/models/ready",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: key, State: proto.MFFailed, TransferID: "current-transfer", Message: "late failure",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelSyncDownloadRequired(FileRef{ModelID: model.ID, NodeID: node.ID}, "current-transfer", "late coordinator failure"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.ModelFileByModelNode(model.ID, node.ID)
	if got.State != proto.MFPresent || got.Downloaded != 1000 || got.Path != "/models/ready" || got.Revision != "abc123" {
		t.Fatalf("ready model regressed: %+v", got)
	}
}

// go test ./internal/server/store -run '^TestModelSyncTimeoutAcceptsLaterFailure$' -count=1
func TestModelSyncTimeoutAcceptsLaterFailure(t *testing.T) {
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&Model{Source: "local", SourceID: "test/timeout", Name: "timeout"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("timeout-node", HashToken("token"), "timeout-node")
	if err != nil {
		t.Fatal(err)
	}
	ref := FileRef{ModelID: model.ID, NodeID: node.ID}
	mf, err := st.PrepareModelPeerSync(ref, "model")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BeginModelSync(ref, 99, "transfer", 1000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelSyncDownloadRequired(ref, "transfer", "target completion timed out"); err != nil {
		t.Fatal(err)
	}
	const failure = "model validation failed"
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: ModelFileKey(mf.ID), State: proto.MFFailed, TransferID: "transfer",
		Acquisition: proto.ModelAcquirePeer, Message: failure,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ModelFileByID(mf.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != proto.MFFailed || got.Message != failure {
		t.Fatalf("timeout hid the later agent failure: %+v", got)
	}
}

func TestExplicitAcquisitionChoiceClearsPeerTransferIdentity(t *testing.T) {
	st, err := Open("file:store_model_sync_choice?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&Model{Source: "huggingface", SourceID: "test/choice", Revision: "main", Name: "choice"})
	node, _ := st.RegisterNode("choice-node", HashToken("token"), "choice-node")
	if _, err := st.UpsertModelFile(&ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "old", State: proto.MFSyncing,
		Acquisition: proto.ModelAcquirePeer, TransferID: "old-transfer", TransferSourceNodeID: 77,
		Downloaded: 500, TransferredBytes: 550, RateBPS: 50, Message: "old",
	}); err != nil {
		t.Fatal(err)
	}
	mf, err := st.PrepareModelDownload(FileRef{ModelID: model.ID, NodeID: node.ID}, "new-path")
	if err != nil {
		t.Fatal(err)
	}
	if mf.State != proto.MFPending || mf.Acquisition != proto.ModelAcquireDownload || mf.TransferID != "" ||
		mf.TransferSourceNodeID != 0 || mf.Downloaded != 0 || mf.TransferredBytes != 0 || mf.RateBPS != 0 || mf.Message != "" {
		t.Fatalf("public download preparation = %+v", mf)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: mfKeyFor(t, st, model.ID, node.ID), State: proto.MFPresent, TransferID: "old-transfer", BytesDone: 999,
	}); err != nil {
		t.Fatal(err)
	}
	mf, _ = st.ModelFileByModelNode(model.ID, node.ID)
	if mf.State != proto.MFPending || mf.Downloaded != 0 {
		t.Fatalf("late peer status contaminated download choice: %+v", mf)
	}
	ds, err := st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ModelFiles) != 1 || ds.ModelFiles[0].Acquisition != proto.ModelAcquireDownload || ds.ModelFiles[0].TransferID != "" {
		t.Fatalf("download desired state = %+v", ds.ModelFiles)
	}
	mf, err = st.RegisterModelFile(FileRef{ModelID: model.ID, NodeID: node.ID}, "/registered/model", 2048)
	if err != nil {
		t.Fatal(err)
	}
	if mf.State != proto.MFRegistered || mf.Acquisition != proto.ModelAcquireRegistered || mf.TransferID != "" ||
		mf.SizeBytes != 2048 || mf.Downloaded != 2048 {
		t.Fatalf("registered model file = %+v", mf)
	}
}

func TestRegisteredModelMayRegressOnlyOnExplicitAbsentInventory(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	model, _ := st.UpsertModel(&Model{Source: "local", SourceID: "registered/absent", Name: "registered"})
	node, _ := st.RegisterNode("registered-absent", HashToken("token"), "registered")
	_, err := st.RegisterModelFile(FileRef{ModelID: model.ID, NodeID: node.ID}, "/models/registered", 100)
	if err != nil {
		t.Fatal(err)
	}
	key := mfKeyFor(t, st, model.ID, node.ID)
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: key, State: proto.MFFailed, Message: "transient"}); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := st.ModelFileByModelNode(model.ID, node.ID)
	if unchanged.State != proto.MFRegistered {
		t.Fatalf("transient status regressed registered model: %+v", unchanged)
	}
	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: key, State: proto.MFAbsent, Acquisition: proto.ModelAcquireRegistered, Message: "path missing"}); err != nil {
		t.Fatal(err)
	}
	absent, _ := st.ModelFileByModelNode(model.ID, node.ID)
	if absent.State != proto.MFAbsent || absent.Message != "path missing" {
		t.Fatalf("absent inventory not applied: %+v", absent)
	}
}

func TestModelFileAcquisitionMigration(t *testing.T) {
	st, err := Open("file:store_model_sync_migration?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 17 {
		t.Fatalf("current migration = %d, want at least 17", version)
	}
	for _, field := range []string{"Acquisition", "TransferID", "TransferSourceNodeID", "TransferredBytes", "RateBPS", "Revision"} {
		if !st.DB().Migrator().HasColumn(&ModelFile{}, field) {
			t.Fatalf("model_files.%s column is missing", field)
		}
	}
}

func TestEvictModelFileRejectsActiveDeploymentAndIsIdempotent(t *testing.T) {
	st, err := Open("file:evict_model_file?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&Model{Source: "huggingface", SourceID: "evict/model", Revision: "main"})
	node, _ := st.RegisterNode("evict-node", HashToken("token"), "evict-node")
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, SizeBytes: 100, Downloaded: 80, Path: "evict"}); err != nil {
		t.Fatal(err)
	}
	deployment, err := st.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CheckModelFileEviction(FileRef{ModelID: model.ID, NodeID: node.ID}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("check error=%v", err)
	} else {
		var referenced *ModelFileReferencedError
		if !errors.As(err, &referenced) || !reflect.DeepEqual(referenced.DeploymentIDs, []uint{deployment.ID}) {
			t.Fatalf("referenced error=%#v", err)
		}
	}
	if err := st.DB().Where("model_id = ?", model.ID).Delete(&Deployment{}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := st.EvictModelFile(FileRef{ModelID: model.ID, NodeID: node.ID})
	if err != nil || got.LogicalBytes != 100 || got.PhysicalBytes != 80 {
		t.Fatalf("eviction=%+v err=%v", got, err)
	}
	got, err = st.EvictModelFile(FileRef{ModelID: model.ID, NodeID: node.ID})
	if err != nil || got.LogicalBytes != 0 {
		t.Fatalf("idempotent eviction=%+v err=%v", got, err)
	}
}

func TestEvictModelFileRejectsDistributedDeploymentParticipant(t *testing.T) {
	st, err := Open("file:evict_model_file_dist?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&Model{Source: "huggingface", SourceID: "evict/dist", Revision: "main"})
	leader, _ := st.RegisterNode("evict-leader", HashToken("leader"), "leader")
	worker, _ := st.RegisterNode("evict-worker", HashToken("worker"), "worker")
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: worker.ID, State: proto.MFPresent, SizeBytes: 200, Downloaded: 180, Path: "evict"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: leader.ID, NodeIDs: "[" + fmt.Sprint(leader.ID) + "," + fmt.Sprint(worker.ID) + "]", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckModelFileEviction(FileRef{ModelID: model.ID, NodeID: worker.ID}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("distributed participant check error=%v", err)
	}
	if _, err := st.EvictModelFile(FileRef{ModelID: model.ID, NodeID: worker.ID}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("distributed participant eviction error=%v", err)
	}
}

func TestEvictModelFileFailsClosedOnMalformedDistributedNodes(t *testing.T) {
	st, err := Open("file:evict_model_file_malformed?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&Model{Source: "huggingface", SourceID: "evict/malformed", Revision: "main"})
	leader, _ := st.RegisterNode("evict-malformed-leader", HashToken("leader"), "leader")
	worker, _ := st.RegisterNode("evict-malformed-worker", HashToken("worker"), "worker")
	if _, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: worker.ID, State: proto.MFPresent, Path: "evict"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: leader.ID, NodeIDs: "not-json", State: "active"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckModelFileEviction(FileRef{ModelID: model.ID, NodeID: worker.ID}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("malformed participant check error=%v", err)
	}
	if _, err := st.EvictModelFile(FileRef{ModelID: model.ID, NodeID: worker.ID}); !errors.Is(err, ErrReferenced) {
		t.Fatalf("malformed participant eviction error=%v", err)
	}
}

// A model served by an HTTP directory carries that directory in every desired
// state, including the one a reconnecting agent replays: the directory is the
// repository, and an agent that received the task without it would have no
// address to download from at all.
func TestDesiredStateCarriesTheDirectoryOfAURLModel(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("directory-node", HashToken("token"), "directory")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&Model{
		Source: "url", SourceID: "Qwen/Qwen3-0.6B", SourceURL: "https://hf-mirror.example.com/Qwen/Qwen3-0.6B", Name: "Qwen3-0.6B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrepareModelDownload(FileRef{ModelID: model.ID, NodeID: node.ID}, "url/Qwen_Qwen3-0.6B-main"); err != nil {
		t.Fatal(err)
	}
	ds, err := st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.ModelFiles) != 1 {
		t.Fatalf("model file tasks = %+v", ds.ModelFiles)
	}
	task := ds.ModelFiles[0]
	if task.Source != "url" || task.Endpoint != model.SourceURL {
		t.Fatalf("task = %+v, want the directory as an explicit endpoint", task)
	}
}

// A pool deployment's replica runs on a node its node list never named; the
// model files that replica uses are still referenced. Once the replica has
// stopped they are not, and a node the pool merely could use never is.
func TestEvictModelFileRejectsWhereAPoolReplicaRuns(t *testing.T) {
	st, err := Open("file:evict_model_file_pool?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&Model{Source: "huggingface", SourceID: "evict/pool", Revision: "main"})
	created, _ := st.RegisterNode("evict-pool-created", HashToken("created"), "created")
	ran, _ := st.RegisterNode("evict-pool-ran", HashToken("ran"), "ran")
	idle, _ := st.RegisterNode("evict-pool-idle", HashToken("idle"), "idle")
	for _, node := range []*Node{ran, idle} {
		if _, err := st.UpsertModelFile(&ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "evict"}); err != nil {
			t.Fatal(err)
		}
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: model.ID, NodeID: created.ID, NodeIDs: "[" + fmt.Sprint(created.ID) + "]", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	replica := Instance{UUID: "pool-replica", DeploymentID: dep.ID, NodeID: ran.ID, State: proto.InstRunning}
	if err := st.DB().Create(&replica).Error; err != nil {
		t.Fatal(err)
	}
	var referenced *ModelFileReferencedError
	if err := st.CheckModelFileEviction(FileRef{ModelID: model.ID, NodeID: ran.ID}); !errors.As(err, &referenced) || len(referenced.DeploymentIDs) != 1 || referenced.DeploymentIDs[0] != dep.ID {
		t.Fatalf("node running a pool replica: err=%v", err)
	}
	if err := st.CheckModelFileEviction(FileRef{ModelID: model.ID, NodeID: idle.ID}); err != nil {
		t.Fatalf("node the pool could use but does not: err=%v", err)
	}
	if err := st.DB().Model(&Instance{}).Where("uuid = ?", replica.UUID).Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := st.EvictModelFile(FileRef{ModelID: model.ID, NodeID: ran.ID}); err != nil {
		t.Fatalf("after the replica stopped: err=%v", err)
	}
}

// mfKeyFor is the protocol key of a model's row on a node in the default
// local storage.
func mfKeyFor(t *testing.T, st *Store, modelID, nodeID uint) string {
	t.Helper()
	mf, err := st.ModelFileAt(FileRef{ModelID: modelID, NodeID: nodeID})
	if err != nil {
		t.Fatal(err)
	}
	return ModelFileKey(mf.ID)
}
