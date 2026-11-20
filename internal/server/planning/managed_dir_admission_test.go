package planning

import (
	"encoding/json"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

type fixedStoragePolicy store.StoragePolicy

func (p fixedStoragePolicy) StoragePolicy() store.StoragePolicy { return store.StoragePolicy(p) }

func managedDirNode(t *testing.T, runtime string, usage ...proto.PathUsage) *store.Node {
	t.Helper()
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	return &store.Node{UUID: "node-1", Name: "gb10-1", RuntimeDriver: runtime, PathUsage: string(raw)}
}

const gib = int64(1) << 30

func TestManagedDirAdmissionChargesWhatSharesTheDisk(t *testing.T) {
	managed := proto.PathUsage{Kind: proto.PathKindManagedDirs, Path: "/var/lib/gpuworks/runtime-mounts/managed-dir", TotalBytes: 4000 * gib, AvailableBytes: 3000 * gib}
	for name, tc := range map[string]struct {
		shares  []string
		policy  int64
		weights int64
		image   int64
		want    managedDirNeed
	}{
		// Its own disk: the directories and the reserve, which defaults to
		// a tenth of the disk capped at 50 GiB.
		"own disk": {weights: 128 * gib, image: 20 * gib,
			want: managedDirNeed{AvailBytes: 3000 * gib, DirBytes: 400 * gib, ReserveBytes: 50 * gib, NeedBytes: 450 * gib}},
		// The storage settings' reserve wins over the default.
		"stated reserve": {policy: 10 * gib,
			want: managedDirNeed{AvailBytes: 3000 * gib, DirBytes: 400 * gib, ReserveBytes: 10 * gib, NeedBytes: 410 * gib}},
		// One disk for everything (a DGX Spark): weights and image too.
		"shared with models and runtime": {shares: []string{"models", "docker"}, weights: 128 * gib, image: 20 * gib,
			want: managedDirNeed{AvailBytes: 3000 * gib, DirBytes: 400 * gib, ReserveBytes: 50 * gib, WeightBytes: 128 * gib, ImageBytes: 20 * gib, NeedBytes: 598 * gib}},
		// The disk is shared with a runtime the node does not use: its image
		// is not unpacked there.
		"shared with another runtime": {shares: []string{"udocker"}, image: 20 * gib,
			want: managedDirNeed{AvailBytes: 3000 * gib, DirBytes: 400 * gib, ReserveBytes: 50 * gib, NeedBytes: 450 * gib}},
	} {
		t.Run(name, func(t *testing.T) {
			usage := managed
			usage.SharesDiskWith = tc.shares
			got := managedDirAdmission(fixedStoragePolicy{ReserveFreeBytes: tc.policy}, managedDirNode(t, "docker", usage), 400*gib, tc.weights, tc.image)
			tc.want.Path = usage.Path
			if got != tc.want {
				t.Fatalf("need = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Nothing declared needs nothing; a node that does not report the disk (an
// agent that predates managed directories) is not checked here.
func TestManagedDirAdmissionWithoutDirsOrWithoutADiskReport(t *testing.T) {
	node := managedDirNode(t, "docker", proto.PathUsage{Kind: "models", Path: "/models", TotalBytes: 100 * gib, AvailableBytes: 10 * gib})
	if got := managedDirAdmission(fixedStoragePolicy{}, node, 0, gib, gib); got != (managedDirNeed{}) {
		t.Fatalf("need without dirs = %+v", got)
	}
	if got := managedDirAdmission(fixedStoragePolicy{}, node, 400*gib, 0, 0); got.NeedBytes != 0 || got.AvailBytes != 0 || got.DirBytes != 400*gib {
		t.Fatalf("need on an unreported disk = %+v", got)
	}
	if got := managedDirAdmission(fixedStoragePolicy{}, &store.Node{PathUsage: "not json"}, 400*gib, 0, 0); got.NeedBytes != 0 {
		t.Fatalf("need on an unreadable report = %+v", got)
	}
}

// A managed directory refusal states the figures it was made of.
func TestManagedDirRefusalStatesItsFigures(t *testing.T) {
	need := managedDirAdmission(fixedStoragePolicy{}, managedDirNode(t, "docker",
		proto.PathUsage{Kind: proto.PathKindManagedDirs, Path: "/state/managed-dir", TotalBytes: 1000 * gib, AvailableBytes: 300 * gib}), 400*gib, 0, 0)
	result := scheduler.Result{Message: scheduler.Reason{Code: scheduler.ReasonInsufficientManagedDirDisk},
		Explain: scheduler.Explain{Filters: []scheduler.Reason{{Code: scheduler.ReasonInsufficientManagedDirDisk}}}}
	in := Input{Node: &store.Node{UUID: "node-1", Name: "gb10-1"}}
	explainManagedDirRefusal(in, need, &result)
	params := result.Message.Params
	if params["required_bytes"] != 450*gib || params["available_bytes"] != 300*gib || params["dir_bytes"] != 400*gib ||
		params["reserve_bytes"] != 50*gib || params["path"] != "/state/managed-dir" || params["node"] != "gb10-1" {
		t.Fatalf("params = %v", params)
	}

	if result.Explain.Filters[0].Params["required_bytes"] != 450*gib {
		t.Fatalf("filter params = %v", result.Explain.Filters[0].Params)
	}
	// Another refusal is left alone.
	other := scheduler.Result{Message: scheduler.Reason{Code: scheduler.ReasonInsufficientDisk}}
	explainManagedDirRefusal(in, need, &other)
	if other.Message.Params != nil {
		t.Fatalf("another refusal got params %v", other.Message.Params)
	}
}

// A stored deployment's launch names its managed directory group from its id
// and creation second; a row never stored names none.
func TestLaunchSourceNamesTheDeploymentsManagedDirGroup(t *testing.T) {
	created := time.Unix(1_700_000_000, 0).UTC()
	if got := LaunchSourceFromDeployment(&store.Deployment{ID: 7, CreatedAt: created}).ManagedDirGroup; got != "7-1700000000" {
		t.Fatalf("group = %q", got)
	}
	if got := LaunchSourceFromDeployment(&store.Deployment{ID: 7}).ManagedDirGroup; got != "" {
		t.Fatalf("unstored row group = %q", got)
	}
}

// Through the whole evaluation: the declared directories are reserved on the
// managed directory disk with the storage settings' reserve, a disk that
// cannot take them refuses the node with the figures, and one that can admits
// it.
func TestEvaluateReservesDeclaredDirectoriesOnTheirDisk(t *testing.T) {
	f := newStorageAdmissionFixture(t)
	if err := f.st.SetStoragePolicy(store.StoragePolicy{ReserveFreeBytes: 5 * gib}); err != nil {
		t.Fatal(err)
	}
	f.outOfTeam.RuntimeDriver = "docker"
	f.outOfTeam.PathUsage = `[{"kind":"managed_dirs","path":"/var/lib/gpuworks/managed-dir","available_bytes":` +
		"107374182400" + `,"total_bytes":4398046511104}]`
	in := Input{Model: f.model, Node: f.outOfTeam, Engine: "vllm", ManagedDirBytes: 100 * gib}
	out := in.Evaluate(f.st)
	if out.Result.OK || out.Result.Message.Code != scheduler.ReasonInsufficientManagedDirDisk {
		t.Fatalf("expected a managed directory refusal: %+v", out.Result)
	}
	if params := out.Result.Message.Params; params["required_bytes"] != 105*gib || params["reserve_bytes"] != 5*gib || params["available_bytes"] != 100*gib {
		t.Fatalf("params = %v", params)
	}
	in.ManagedDirBytes = 90 * gib
	if out := in.Evaluate(f.st); out.Result.Message.Code == scheduler.ReasonInsufficientManagedDirDisk {
		t.Fatalf("room for the directories was refused: %+v", out.Result)
	}
	// The KV disk tier lives on the same disk and counts with the dirs: a
	// stated 20 GiB tier on top of 90 GiB of dirs no longer fits.
	in.Quick = deploy.LaunchConfig{SharedKVDir: "/kv", KVDiskLimitGiB: 20}
	if out := in.Evaluate(f.st); out.Result.Message.Code != scheduler.ReasonInsufficientManagedDirDisk || out.Result.Message.Params["dir_bytes"] != 110*gib {
		t.Fatalf("the KV disk tier was not charged with the dirs: %+v", out.Result)
	}
	in.Quick = deploy.LaunchConfig{}
	// Planning as if nothing ran on the node (can it ever fit?) waits for a
	// full disk instead of refusing it.
	in.ManagedDirBytes, in.Unloaded = 100*gib, true
	if out := in.Evaluate(f.st); out.Result.Message.Code == scheduler.ReasonInsufficientManagedDirDisk {
		t.Fatalf("an unloaded plan refused a full disk: %+v", out.Result)
	}
}
