package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// The database lives in a tree shared with the operator, so opening it makes
// it private to its owner and trusted group (0600) — the install and reset commands run without sudo.
func TestOpenMakesTheDatabaseGroupWritable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	if _, err := Open(path, bus.New()); err != nil {
		t.Fatalf("open: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadOnlyDSN(t *testing.T) {
	const params = "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	for dsn, want := range map[string]string{
		"/var/gpuworks/gpuworks.db":                   "file:/var/gpuworks/gpuworks.db?" + params,
		"file:/var/gpuworks/gpuworks.db":              "file:/var/gpuworks/gpuworks.db?" + params,
		"file:/var/gpuworks/gpuworks.db?cache=shared": "file:/var/gpuworks/gpuworks.db?cache=shared&" + params,
	} {
		if got := readOnlyDSN(dsn); got != want {
			t.Fatalf("readOnlyDSN(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestDBFilePath(t *testing.T) {
	for dsn, want := range map[string]string{
		"/var/gpuworks/gpuworks.db":                   "/var/gpuworks/gpuworks.db",
		"file:/var/gpuworks/gpuworks.db?cache=shared": "/var/gpuworks/gpuworks.db",
		":memory:":                          "",
		"":                                  "",
		"file:mem?mode=memory&cache=shared": "",
		"file:/tmp/private%20database.db?cache=shared": "/tmp/private database.db",
		"file://localhost/tmp/private.db":              "/tmp/private.db",
	} {
		if got := dbFilePath(dsn); got != want {
			t.Fatalf("dbFilePath(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// Unique in-memory DB per test via a distinct shared-cache name.
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	s, err := Open(dsn, bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return s
}

func TestRegisterAndAuth(t *testing.T) {
	s := newTestStore(t)
	tok := "node-token-xyz"
	n, err := s.RegisterNode("uuid-1", HashToken(tok), "host-a")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if n.State != NodeStateConnecting {
		t.Errorf("new node state = %q, want pending", n.State)
	}
	if n.ConnectMode != "agent_dial" {
		t.Errorf("new node connect mode = %q, want agent_dial", n.ConnectMode)
	}
	got, err := s.NodeByUUID("uuid-1")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if got.TokenHash != HashToken(tok) {
		t.Error("token hash mismatch")
	}
	if _, err := s.NodeByUUID("nope"); err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// The weight backfill is one column write and nothing else: a caller that has
// neither a row nor a figure leaves the store alone, and a real figure is what
// the next estimate reads.
func TestSetModelWeightBytesBackfillsOneRow(t *testing.T) {
	st, err := Open("file:store_set_model_weight_bytes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&Model{Source: "huggingface", SourceID: "acme/backfill", Name: "backfill"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetModelWeightBytes(0, 1<<30); err != nil {
		t.Fatalf("a row id of zero must be a no-op: %v", err)
	}
	if err := st.SetModelWeightBytes(model.ID, 0); err != nil {
		t.Fatalf("a weight of zero must be a no-op: %v", err)
	}
	if stored, err := st.GetModel(model.ID); err != nil || stored.WeightBytes != 0 {
		t.Fatalf("no-op writes changed the row: %+v err=%v", stored, err)
	}
	if err := st.SetModelWeightBytes(model.ID, 4<<30); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetModel(model.ID)
	if err != nil || stored.WeightBytes != 4<<30 {
		t.Fatalf("stored weight = %+v err=%v", stored, err)
	}
}

func TestUpsertModelNormalizesSourceAndCaseFoldsSourceID(t *testing.T) {
	s := newTestStore(t)
	first, err := s.UpsertModel(&Model{Source: " HuggingFace ", SourceID: " Qwen/Qwen3-8B ", Revision: "main", Name: "first"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "qwen/qwen3-8b", Revision: "main", Name: "updated"})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || second.Name != "updated" || second.Source != "huggingface" {
		t.Fatalf("models were not normalized/upserted: first=%+v second=%+v", first, second)
	}
	otherSource, err := s.UpsertModel(&Model{Source: "modelscope", SourceID: "qwen/qwen3-8b", Revision: "main", Name: "other source"})
	if err != nil {
		t.Fatal(err)
	}
	if otherSource.ID == first.ID {
		t.Fatal("models from different sources were incorrectly merged")
	}
}

func TestFindModelBySourceRevisionNeverFallsBack(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	main, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "Org/Model", Revision: "main", Name: "main"})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "org/model", Revision: "abc123", Name: "commit"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.FindModelBySourceRevision("ORG/MODEL", "HuggingFace", "abc123")
	if err != nil || got.ID != commit.ID || got.ID == main.ID {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if _, err := s.FindModelBySourceRevision("org/model", "huggingface", "missing"); err != ErrNotFound {
		t.Fatalf("missing revision error = %v", err)
	}
}

func TestModelArtifactSubfolderIsPartOfImmutableIdentity(t *testing.T) {
	s := newTestStore(t)
	whole, err := s.UpsertModel(&Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit", Name: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	fl2va, err := s.UpsertModel(&Model{Source: "modelscope", SourceID: "minimax/minimax-h3", Revision: "commit", ArtifactSubfolder: " FL2VA ", Name: "fl2va"})
	if err != nil {
		t.Fatal(err)
	}
	if whole.ID == fl2va.ID || fl2va.ArtifactSubfolder != "FL2VA" {
		t.Fatalf("whole=%+v subset=%+v", whole, fl2va)
	}
	got, err := s.FindModelArtifact("MINIMAX/MINIMAX-H3", "ModelScope", "commit", "FL2VA")
	if err != nil || got.ID != fl2va.ID {
		t.Fatalf("artifact lookup=%+v err=%v", got, err)
	}
	if got, err := s.FindModelBySourceRevision("MiniMax/MiniMax-H3", "modelscope", "commit"); err != nil || got.ID != whole.ID {
		t.Fatalf("whole-repository lookup=%+v err=%v", got, err)
	}
	if _, err := s.UpsertModel(&Model{Source: "modelscope", SourceID: "MiniMax/MiniMax-H3", Revision: "commit", ArtifactSubfolder: "../escape"}); err == nil {
		t.Fatal("unsafe artifact subfolder was accepted")
	}
}

func TestDeleteModelRejectsDeploymentReferenceAndCleansDerivedRows(t *testing.T) {
	s := newTestStore(t)
	model, err := s.UpsertModel(&Model{Source: "huggingface", SourceID: "org/delete", Revision: "main", Name: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&ModelFile{ModelID: model.ID, NodeID: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Create(&Profile{Fingerprint: "delete-profile", ModelID: model.ID}).Error; err != nil {
		t.Fatal(err)
	}
	dep, err := s.CreateDeployment(&Deployment{ModelID: model.ID, State: "stopped"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(model.ID); !errors.Is(err, ErrReferenced) {
		t.Fatalf("DeleteModel error = %v, want ErrReferenced", err)
	}
	if err := s.DB().Delete(&Deployment{}, dep.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteModel(model.ID); err != nil {
		t.Fatal(err)
	}
	var models, files, profiles int64
	if err := s.DB().Model(&Model{}).Where("id = ?", model.ID).Count(&models).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&ModelFile{}).Where("model_id = ?", model.ID).Count(&files).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB().Model(&Profile{}).Where("model_id = ?", model.ID).Count(&profiles).Error; err != nil {
		t.Fatal(err)
	}
	if models != 0 || files != 0 || profiles != 0 {
		t.Fatalf("remaining model/files/profiles = %d/%d/%d", models, files, profiles)
	}
}

func TestRegisterNodeStoresConnectMode(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("uuid-connect", HashToken("t"), "host", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	if n.ConnectMode != "server_dial" {
		t.Fatalf("ConnectMode = %q", n.ConnectMode)
	}
	if err := s.SetNodeServerDialAddress(n.UUID, "10.0.0.8:19090"); err != nil {
		t.Fatal(err)
	}
	nodes, err := s.ServerDialNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].UUID != n.UUID || nodes[0].ServerDialAddress != "10.0.0.8:19090" {
		t.Fatalf("ServerDialNodes = %+v", nodes)
	}
}

func TestCreateNodePendingGeneratesReusableToken(t *testing.T) {
	s := newTestStore(t)
	n, token, err := s.CreateNodePending("display-name")
	if err != nil {
		t.Fatal(err)
	}
	if n.State != NodeStateConnecting || n.Name != "display-name" || token == "" || n.Token != token || n.TokenHash != HashToken(token) {
		t.Fatalf("pending node = %+v token=%q", n, token)
	}
	got, err := s.NodeByTokenHash(HashToken(token))
	if err != nil || got.UUID != n.UUID {
		t.Fatalf("NodeByTokenHash = %+v, %v", got, err)
	}
}

func TestRegenerateNodeTokenInvalidatesPreviousHash(t *testing.T) {
	s := newTestStore(t)
	n, oldToken, err := s.CreateNodePending("rotate-token")
	if err != nil {
		t.Fatal(err)
	}
	got, newToken, err := s.RegenerateNodeToken(n.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if newToken == "" || newToken == oldToken || got.Token != newToken || got.TokenHash != HashToken(newToken) {
		t.Fatalf("regenerated node=%+v token=%q", got, newToken)
	}
	if _, err := s.NodeByTokenHash(HashToken(oldToken)); err != ErrNotFound {
		t.Fatalf("old token lookup err=%v, want ErrNotFound", err)
	}
}

func TestUpdateNodeStatusReconcilesGPUs(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-2", HashToken("t"), "host-b"); err != nil {
		t.Fatal(err)
	}

	// First report: two GPUs.
	st := proto.NodeStatus{
		Sys: proto.SysInfo{
			Hostname: "host-b", CPUCores: 32, MemTotalKB: 64 * 1024 * 1024,
			GPUVendor: "nvidia", GPUDriver: "550.54.15", NvidiaDriver: "550.54.15",
			RuntimeDriver: "docker", RuntimePlugin: "NVIDIA Container Toolkit 1.20.0", Runtimes: []string{"docker"}, PartitionAssets: []proto.AssetReport{{Backend: "nvidia-dgpu", Root: "embedded://dgpu", Files: []string{"libvgpu.so"}}},
			PartitionProbes: []proto.PartitionProbeResult{{Backend: "nvidia-dgpu", Status: proto.PartitionProbePassed}}, PartitionSafetyProtocol: 1,
		},
		GPUs: []proto.GPUInfo{
			{Index: 0, UUID: "g0", Name: "H100", MemoryTotalBytes: 80 << 30, ComputeCapability: "9.0", Vendor: "nvidia", ArchID: "sm_90", Healthy: true},
			{Index: 1, UUID: "g1", Name: "H100", MemoryTotalBytes: 80 << 30, ComputeCapability: "9.0", Vendor: "nvidia", ArchID: "sm_90", MIGEnabled: true, Healthy: true},
		},
	}
	if err := s.UpdateNodeStatus("uuid-2", st); err != nil {
		t.Fatalf("status: %v", err)
	}
	n, err := s.NodeWithGPUs("uuid-2")
	if err != nil {
		t.Fatal(err)
	}
	if n.State != NodeStateOnline {
		t.Errorf("state = %q, want online", n.State)
	}
	if n.GPUCount != 2 || len(n.GPUs) != 2 {
		t.Fatalf("GPUCount=%d len=%d, want 2", n.GPUCount, len(n.GPUs))
	}
	// The sm_XY the agent sends follows from the capability and is not kept.
	if n.GPUs[0].Vendor != "nvidia" || n.GPUs[0].ArchID != "" || n.GPUs[0].ComputeCapability != "9.0" {
		t.Fatalf("GPU vendor/arch_id/cc = %q/%q/%q, want nvidia//9.0", n.GPUs[0].Vendor, n.GPUs[0].ArchID, n.GPUs[0].ComputeCapability)
	}
	if arch := n.ArchOf(n.GPUs[0]); arch.ID != "sm_90" || arch.Family != "hopper" {
		t.Fatalf("ArchOf = %+v, want sm_90/hopper", arch)
	}
	if !n.GPUs[1].MIGEnabled {
		t.Fatalf("legacy MIG flag was not persisted: %+v", n.GPUs[1])
	}
	if n.GPUVendor != "nvidia" || n.GPUDriver != "550.54.15" || n.NvidiaDriver != "550.54.15" {
		t.Fatalf("node gpu vendor/driver/nvidia_driver = %q/%q/%q", n.GPUVendor, n.GPUDriver, n.NvidiaDriver)
	}
	if n.RuntimeDriver != "docker" || n.Runtimes != `["docker"]` || !strings.Contains(n.PartitionAssets, "nvidia-dgpu") || !strings.Contains(n.PartitionProbes, "passed") {
		t.Fatalf("node runtime/assets/probes = %q/%q/%q/%q", n.RuntimeDriver, n.Runtimes, n.PartitionAssets, n.PartitionProbes)
	}
	if n.RuntimePlugin != "NVIDIA Container Toolkit 1.20.0" {
		t.Fatalf("node runtime plugin = %q", n.RuntimePlugin)
	}
	if n.PartitionSafetyProtocol != 1 {
		t.Fatalf("partition safety protocol = %d", n.PartitionSafetyProtocol)
	}

	// Second report: one GPU removed and remaining metadata updated. Reconcile
	// must delete the stale row and persist name/arch updates, the name
	// normalized like every stored name.
	st.GPUs = st.GPUs[:1]
	st.Sys.PartitionSafetyProtocol = 0 // Rolling back to an old agent must revoke readiness.
	st.GPUs[0].Name = "NVIDIA H100 NVL"
	st.GPUs[0].ComputeCapability = "10.0"
	if err := s.UpdateNodeStatus("uuid-2", st); err != nil {
		t.Fatal(err)
	}
	n, _ = s.NodeWithGPUs("uuid-2")
	if n.PartitionSafetyProtocol != 0 {
		t.Fatalf("stale partition safety protocol = %d", n.PartitionSafetyProtocol)
	}
	if len(n.GPUs) != 1 {
		t.Fatalf("after shrink len=%d, want 1", len(n.GPUs))
	}
	if n.GPUs[0].Index != 0 {
		t.Errorf("kept wrong GPU index %d", n.GPUs[0].Index)
	}
	if n.GPUs[0].Name != "H100 NVL" || n.GPUs[0].ComputeCapability != "10.0" {
		t.Errorf("updated name/cc = %q/%q, want H100 NVL/10.0", n.GPUs[0].Name, n.GPUs[0].ComputeCapability)
	}
	if arch := n.ArchOf(n.GPUs[0]); arch.ID != "sm_100" || arch.Family != "blackwell" {
		t.Errorf("updated arch = %+v", arch)
	}
}

func TestUpdateNodeStatusKeepsInventoryWhenGPUProbeFails(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-probe", HashToken("t"), "probe-host"); err != nil {
		t.Fatal(err)
	}
	ok := true
	if err := s.UpdateNodeStatus("uuid-probe", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{{Index: 0, UUID: "ppu-0", Vendor: "ppu", Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	failed := false
	if err := s.UpdateNodeStatus("uuid-probe", proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "ppu-smi timeout"}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NodeWithGPUs("uuid-probe")
	if err != nil {
		t.Fatal(err)
	}
	// The agent is still on the other end, so the node stays online: a node
	// whose driver stopped answering is the one an operator most needs a
	// terminal on, and the terminal follows the connection state.
	if n.State != NodeStateOnline || len(n.GPUs) != 1 || n.GPUs[0].UUID != "ppu-0" {
		t.Fatalf("failed probe changed the connection state or the inventory: state=%q gpus=%+v", n.State, n.GPUs)
	}
	if n.GPUProbeError != "ppu-smi timeout" || n.GPUProbeFailedAt == nil {
		t.Fatalf("failed probe left no record of itself: err=%q at=%v", n.GPUProbeError, n.GPUProbeFailedAt)
	}
	// Another failure keeps when the failing started, so the record says
	// for how long, and takes the latest reason.
	since := *n.GPUProbeFailedAt
	time.Sleep(5 * time.Millisecond)
	if err := s.UpdateNodeStatus("uuid-probe", proto.NodeStatus{GPUProbeOK: &failed, GPUProbeError: "ppu-smi: no answer"}); err != nil {
		t.Fatal(err)
	}
	if n, err = s.NodeWithGPUs("uuid-probe"); err != nil {
		t.Fatal(err)
	}
	if n.GPUProbeFailedAt == nil || !n.GPUProbeFailedAt.Equal(since) || n.GPUProbeError != "ppu-smi: no answer" {
		t.Fatalf("second failure: at=%v (first %v) err=%q", n.GPUProbeFailedAt, since, n.GPUProbeError)
	}

	// A probe that succeeds again clears the record.
	if err := s.UpdateNodeStatus("uuid-probe", proto.NodeStatus{GPUProbeOK: &ok, GPUs: []proto.GPUInfo{{Index: 0, UUID: "ppu-0", Vendor: "ppu", Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	if n, err = s.NodeWithGPUs("uuid-probe"); err != nil {
		t.Fatal(err)
	}
	if n.GPUProbeError != "" || n.GPUProbeFailedAt != nil {
		t.Fatalf("a successful probe kept the earlier failure: err=%q at=%v", n.GPUProbeError, n.GPUProbeFailedAt)
	}
}

func TestUpdateNodeStatusMatchesPPUByUUIDAcrossIndexReenumeration(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-reindex", HashToken("t"), "reindex-host"); err != nil {
		t.Fatal(err)
	}
	status := func(gpus []proto.GPUInfo) {
		t.Helper()
		ok := true
		if err := s.UpdateNodeStatus("uuid-reindex", proto.NodeStatus{GPUProbeOK: &ok, GPUs: gpus}); err != nil {
			t.Fatal(err)
		}
	}
	status([]proto.GPUInfo{{Index: 0, UUID: "stable-a", Vendor: "ppu"}, {Index: 1, UUID: "stable-b", Vendor: "ppu"}, {Index: 2, UUID: "stable-c", Vendor: "ppu"}})
	status([]proto.GPUInfo{{Index: 0, UUID: "stable-a", Vendor: "ppu"}, {Index: 1, UUID: "stable-c", Vendor: "ppu"}})
	n, err := s.NodeWithGPUs("uuid-reindex")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.GPUs) != 2 {
		t.Fatalf("GPUs = %+v", n.GPUs)
	}
	for _, g := range n.GPUs {
		if g.UUID == "stable-c" && g.Index != 1 {
			t.Fatalf("stable-c index = %d, want 1", g.Index)
		}
	}
}

func TestUpdateNodeStatusPersistsGPUHealthTransitionsAndClearsDetails(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-health", HashToken("t"), "health-host"); err != nil {
		t.Fatal(err)
	}
	report := func(gpu proto.GPUInfo) GPU {
		t.Helper()
		if err := s.UpdateNodeStatus("uuid-health", proto.NodeStatus{GPUs: []proto.GPUInfo{gpu}}); err != nil {
			t.Fatal(err)
		}
		node, err := s.NodeWithGPUs("uuid-health")
		if err != nil {
			t.Fatal(err)
		}
		if len(node.GPUs) != 1 {
			t.Fatalf("GPUs = %+v", node.GPUs)
		}
		return node.GPUs[0]
	}

	base := proto.GPUInfo{Index: 4, UUID: "ascend-4", Name: "Ascend 910B4", Vendor: "ascend", Healthy: true, HealthStatus: "OK"}
	if got := report(base); !got.Healthy() || got.HealthStatus != "OK" || !got.CanTakeWork() {
		t.Fatalf("initial health = %+v", got)
	}

	// A warning the vendor tool found: the finding is kept and shown, and
	// the card keeps serving — that is what a warning means.
	base.Healthy = true
	base.HealthStatus = "Warning"
	base.HealthCode = "81078603"
	base.HealthMessage = "port link status change(up to down)"
	if got := report(base); !got.Healthy() || got.HealthStatus != "Warning" || got.HealthCode != "81078603" || got.HealthMessage == "" || !got.CanTakeWork() {
		t.Fatalf("warning health = %+v", got)
	}

	// A fault: the card stops taking work, and the finding says why.
	base.Healthy = false
	base.HealthStatus = "Error"
	base.HealthCode = "memory_uncorrectable_errors"
	if got := report(base); got.Healthy() || got.HealthStatus != "Error" || got.CanTakeWork() {
		t.Fatalf("fault health = %+v", got)
	}

	base.Healthy = true
	base.HealthStatus = "OK"
	base.HealthCode = ""
	base.HealthMessage = ""
	if got := report(base); !got.Healthy() || got.HealthStatus != "OK" || got.HealthCode != "" || got.HealthMessage != "" {
		t.Fatalf("recovered health = %+v", got)
	}

	// An agent that predates health levels states only the boolean, and
	// the level it is stored at is derived from that once, here.
	old := proto.GPUInfo{Index: 4, UUID: "ascend-4", Name: "Ascend 910B4", Vendor: "ascend"}
	if got := report(old); got.Healthy() || got.HealthStatus != "Error" || got.CanTakeWork() {
		t.Fatalf("an old agent reporting an unusable card = %+v", got)
	}
	old.Healthy = true
	if got := report(old); !got.Healthy() || got.HealthStatus != "OK" {
		t.Fatalf("an old agent reporting a good card = %+v", got)
	}
}

func TestUpdateNodeStatusDerivesNeutralDriverForOldAndNonNVIDIAReports(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-old", HashToken("t"), "host-old"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-old", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "host-old", NvidiaDriver: "535.00"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NodeWithGPUs("uuid-old")
	if err != nil {
		t.Fatal(err)
	}
	if n.GPUVendor != "nvidia" || n.GPUDriver != "535.00" || n.NvidiaDriver != "535.00" {
		t.Fatalf("old report mapped to %q/%q/%q", n.GPUVendor, n.GPUDriver, n.NvidiaDriver)
	}

	if _, err := s.RegisterNode("uuid-asc", HashToken("t"), "host-asc"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-asc", proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "host-asc", GPUVendor: "ascend", GPUDriver: "npu-smi 26.0.rc1"},
		GPUs: []proto.GPUInfo{{
			Index: 2, UUID: "ascend-2", Name: "Ascend 910B4", Vendor: "ascend", ArchID: "910B4", Healthy: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err = s.NodeWithGPUs("uuid-asc")
	if err != nil {
		t.Fatal(err)
	}
	if n.GPUVendor != "ascend" || n.GPUDriver != "26.0.RC1" || n.NvidiaDriver != "" {
		t.Fatalf("ascend report mapped to %q/%q/%q", n.GPUVendor, n.GPUDriver, n.NvidiaDriver)
	}
}

func TestUpdateNodeStatusPersistsLegacyMIGFlagWithoutPartitionClaim(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.RegisterNode("uuid-mig", HashToken("t"), "host-mig"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-mig", proto.NodeStatus{
		GPUs: []proto.GPUInfo{{Index: 0, UUID: "g0", Name: "H100", MIGEnabled: true, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.NodeWithGPUs("uuid-mig")
	if err != nil {
		t.Fatal(err)
	}
	if len(n.GPUs) != 1 || !n.GPUs[0].MIGEnabled {
		t.Fatalf("legacy MIG flag = %+v", n.GPUs)
	}
}

func TestSetNodeState(t *testing.T) {
	s := newTestStore(t)
	s.RegisterNode("uuid-3", HashToken("t"), "h")
	if err := s.SetNodeState("uuid-3", NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	n, _ := s.NodeByUUID("uuid-3")
	if n.State != NodeStateOffline {
		t.Errorf("state = %q, want offline", n.State)
	}
}

func TestMarkStaleOfflinePublishesNodeEvent(t *testing.T) {
	b := bus.New()
	s, err := Open("file:stale_offline_event?mode=memory&cache=shared", b)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	node, err := s.RegisterNode("uuid-stale-event", HashToken("token"), "stale")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-10 * time.Minute)
	if err := s.DB().Model(&Node{}).Where("id = ?", node.ID).Updates(map[string]any{
		"state": NodeStateOnline, "last_seen_at": old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	sub := b.Subscribe(4, "node")
	defer sub.Close()
	if err := s.MarkStaleOffline(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-sub.C():
		if event.Key != node.UUID || event.Reason != "state:offline" {
			t.Fatalf("stale event = %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("stale offline transition did not publish a node event")
	}
}

func TestDeleteNodeCleansLinkedState(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("uuid-delete", HashToken("t"), "delete-me")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNodeStatus("uuid-delete", proto.NodeStatus{
		GPUs: []proto.GPUInfo{{Index: 0, UUID: "gpu-delete", Name: "H100", Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertModelFile(&ModelFile{ModelID: 1, NodeID: n.ID, State: "present"}); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProvider(&Provider{Name: "provider", Kind: "test", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProviderInstance(&ProviderInstance{ProviderID: p.ID, ExternalID: "inst-delete", NodeID: n.ID}); err != nil {
		t.Fatal(err)
	}
	tok, err := s.CreateProviderBootstrapToken(p.ID, "inst-delete", "bootstrap-delete", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetProviderBootstrapTokenNode(tok.ID, n.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteNode("uuid-delete"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if _, err := s.NodeByUUID("uuid-delete"); err != ErrNotFound {
		t.Fatalf("NodeByUUID err = %v, want ErrNotFound", err)
	}
	for name, model := range map[string]any{
		"gpus":                      &GPU{},
		"model_files":               &ModelFile{},
		"provider_bootstrap_tokens": &ProviderBootstrapToken{},
	} {
		var count int64
		if err := s.DB().Model(model).Where("node_id = ?", n.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s linked rows = %d, want 0", name, count)
		}
	}
	var pi ProviderInstance
	if err := s.DB().Where("provider_id = ? AND external_id = ?", p.ID, "inst-delete").First(&pi).Error; err != nil {
		t.Fatal(err)
	}
	if pi.NodeID != 0 {
		t.Fatalf("ProviderInstance.NodeID = %d, want 0", pi.NodeID)
	}
}

func TestDeleteProviderCleansLinkedInstancesAndTokens(t *testing.T) {
	s := newTestStore(t)
	p, err := s.CreateProvider(&Provider{Name: "provider", Kind: "test", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertProviderInstance(&ProviderInstance{ProviderID: p.ID, ExternalID: "inst-clean"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProviderBootstrapToken(p.ID, "inst-clean", "bootstrap-clean", time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteProvider(p.ID); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if _, err := s.GetProvider(p.ID); err != ErrNotFound {
		t.Fatalf("GetProvider err = %v, want ErrNotFound", err)
	}
	var instCount int64
	if err := s.DB().Model(&ProviderInstance{}).Where("provider_id = ?", p.ID).Count(&instCount).Error; err != nil {
		t.Fatal(err)
	}
	if instCount != 0 {
		t.Fatalf("provider instances = %d, want 0", instCount)
	}
	var tokCount int64
	if err := s.DB().Model(&ProviderBootstrapToken{}).Where("provider_id = ?", p.ID).Count(&tokCount).Error; err != nil {
		t.Fatal(err)
	}
	if tokCount != 0 {
		t.Fatalf("provider bootstrap tokens = %d, want 0", tokCount)
	}
}

func TestDeleteNodeRejectsNodeWithInstances(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("uuid-busy", HashToken("t"), "busy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateInstance(&Instance{UUID: "inst-busy", NodeID: n.ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	err = s.DeleteNode("uuid-busy")
	if err == nil || !strings.Contains(err.Error(), "has 1 instance") {
		t.Fatalf("DeleteNode err = %v", err)
	}
	if _, err := s.NodeByUUID("uuid-busy"); err != nil {
		t.Fatalf("node should remain after rejected delete: %v", err)
	}
}
