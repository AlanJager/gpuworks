package planning

import (
	"errors"
	"strconv"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type storageAdmissionFixture struct {
	st         *store.Store
	team       *store.Storage
	model      *store.Model
	inTeam     *store.Node
	outOfTeam  *store.Node
	localModel *store.ModelFile
}

// A team NFS that applies to one node, and a second node with the model in
// its local storage and almost no free disk: counting a download there would
// refuse it for space.
func newStorageAdmissionFixture(t *testing.T) *storageAdmissionFixture {
	t.Helper()
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &storageAdmissionFixture{st: st}
	for i, name := range []string{"in-team", "out-of-team"} {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		node.PathUsage = `[{"kind":"models","available_bytes":1048576}]`
		node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 40 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
		if i == 0 {
			f.inTeam = node
		} else {
			f.outOfTeam = node
		}
	}
	if f.team, err = st.CreateStorage(&store.Storage{Name: "lab-nfs", Type: proto.StorageNFS, Enabled: true, Path: "/mnt/gpuworks/lab-nfs", Export: "h:/lab"},
		[]store.StorageNode{{NodeID: f.inTeam.ID}}); err != nil {
		t.Fatal(err)
	}
	if f.model, err = st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2.5-0.5B-Instruct", Name: "q05", WeightBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: f.team.ID, Path: "Qwen/Qwen2.5-0.5B-Instruct", State: proto.MFPresent, SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	if f.localModel, err = st.UpsertModelFile(&store.ModelFile{ModelID: f.model.ID, StorageID: st.DefaultLocalStorageID(), NodeID: f.outOfTeam.ID,
		Path: "/opt/gpuworks/models/q05", State: proto.MFPresent, SizeBytes: 1 << 30, Downloaded: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *storageAdmissionFixture) evaluate(node *store.Node, pins []uint) Output {
	return (Input{Model: f.model, Node: node, Engine: "vllm", Quick: deploy.LaunchConfig{GPUMemUtil: 0.8},
		ModelRules: store.SourceRules{SaveTo: f.team.ID, Pinned: pins}}).Evaluate(f.st)
}

// A deployment that saves into the team NFS still fits a node outside it that
// holds the model locally, unless it is limited to that storage.
func TestAdmissionFollowsTheCopyTheNodeReads(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	if out := f.evaluate(f.outOfTeam, nil); !out.Result.OK {
		t.Fatalf("node with a local copy refused: %+v", out.Result)
	}
	out := f.evaluate(f.outOfTeam, []uint{f.team.ID})
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonStorageUnavailable || out.Result.Message.Params["reason"] != store.LocationStorageNotOnNode || out.Result.Message.Params["storage"] != "lab-nfs" {
		t.Fatalf("pinned to a storage the node is not in: %+v", out.Result)
	}
	// Inside the team the NFS is not mounted yet: a reason to wait, named.
	out = f.evaluate(f.inTeam, []uint{f.team.ID})
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonStorageUnavailable || out.Result.Message.Params["reason"] != store.LocationStorageUnavailable {
		t.Fatalf("pinned, not mounted: %+v", out.Result)
	}
	if err := f.st.ApplyStorageStatus(f.inTeam.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: f.team.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/lab-nfs"}}}); err != nil {
		t.Fatal(err)
	}
	if out := f.evaluate(f.inTeam, []uint{f.team.ID}); !out.Result.OK {
		t.Fatalf("pinned, mounted: %+v", out.Result)
	}
	// Without the local copy the node outside the team has to download into
	// its default storage, which has no room.
	if err := f.st.DeleteModelFileRow(f.localModel.ID); err != nil {
		t.Fatal(err)
	}
	if out := f.evaluate(f.outOfTeam, nil); out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientDisk {
		t.Fatalf("download into a full disk: %+v", out.Result)
	}
}

func TestStorageLabelsAndRequirements(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	nodes := []*store.Node{f.inTeam, f.outOfTeam}
	scope, err := LoadStorageScope(f.st, nodes)
	if err != nil {
		t.Fatal(err)
	}
	team := strconv.FormatUint(uint64(f.team.ID), 10)
	local := strconv.FormatUint(uint64(f.st.DefaultLocalStorageID()), 10)
	facts := NodeFacts{Storages: scope}
	in := SystemLabelValues(SystemLabels(f.inTeam, facts), nodelabel.KeyStorage)
	out := SystemLabelValues(SystemLabels(f.outOfTeam, facts), nodelabel.KeyStorage)
	if len(in) != 2 || len(out) != 1 || out[0] != local {
		t.Fatalf("storage labels: in-team %v, out-of-team %v", in, out)
	}
	if display, _ := displayOf(SystemLabels(f.inTeam, facts), nodelabel.KeyStorage, team); display != "lab-nfs" {
		t.Fatalf("display = %q", display)
	}
	reqs := nodelabel.Requirements{nodelabel.KeyStorage: {team}}
	if !RequirementsNeedStorages(reqs) || RequirementsNeedStorages(nodelabel.Requirements{"pool": {"a"}}) {
		t.Fatal("RequirementsNeedStorages")
	}
	if err := MatchRequirements(f.inTeam, facts, reqs); err != nil {
		t.Fatalf("in-team: %v", err)
	}
	if err := MatchRequirements(f.outOfTeam, facts, reqs); err == nil {
		t.Fatal("a node the storage does not apply to matched")
	}
	// Unloaded storages fail closed.
	if err := MatchRequirements(f.inTeam, NodeFacts{}, reqs); err == nil {
		t.Fatal("a storage requirement matched without the storages loaded")
	}
	labels := RequirementLabels(reqs, nodes, facts)
	if len(labels) != 1 || labels[0].Display != "lab-nfs" || labels[0].Nodes != 1 {
		t.Fatalf("requirement labels = %+v", labels)
	}
	if !nodelabel.IsSystemKey(nodelabel.KeyStorage) {
		t.Fatal("the storage key is not a system key a requirement may name")
	}
}

// LoadNodeFacts loads only what the requirements read, and a part it cannot
// load stays nil so the requirement is unmet rather than matched.
func TestLoadNodeFactsLoadsOnlyWhatTheRequirementsRead(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	nodes := []store.Node{*f.inTeam, *f.outOfTeam}
	resolved := 0
	resolve := func([]store.Node) (netplan.Resolution, error) { resolved++; return netplan.Resolution{}, nil }
	if facts := LoadNodeFacts(f.st, resolve, nodes, nodelabel.Requirements{"pool": {"a"}}); facts.Networks != nil || facts.Storages != nil || resolved != 0 {
		t.Fatalf("written-label requirement loaded %+v (resolve called %d)", facts, resolved)
	}
	team := strconv.FormatUint(uint64(f.team.ID), 10)
	facts := LoadNodeFacts(f.st, resolve, nodes, nodelabel.Requirements{nodelabel.KeyStorage: {team}})
	if resolved != 0 || len(facts.Storages[f.inTeam.ID]) != 2 || len(facts.Storages[f.outOfTeam.ID]) != 1 {
		t.Fatalf("storage requirement loaded %+v (resolve called %d)", facts, resolved)
	}
	if MatchRequirements(f.inTeam, facts, nodelabel.Requirements{nodelabel.KeyStorage: {team}}) != nil {
		t.Fatal("the node in the team storage does not match")
	}
	failing := func([]store.Node) (netplan.Resolution, error) { return nil, errors.New("unreadable") }
	facts = LoadNodeFacts(f.st, failing, nodes, nodelabel.Requirements{nodelabel.KeyComputeNetwork: {"1"}})
	if facts.Networks != nil {
		t.Fatalf("an unresolvable network set loaded %+v", facts.Networks)
	}
}
