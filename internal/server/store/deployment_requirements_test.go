package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

// A deployment and its requirements are one write: the requirements come
// back normalized, and the deployment defaults to the spread strategy.
func TestCreateDeploymentWithRequirements(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "a"}, nodelabel.Requirements{
		nodelabel.KeyVendor: {"nvidia"},
		nodelabel.KeyNode:   {"n-2", "n-1", "n-2"},
		"pool":              {"inference"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PlacementStrategy != PlacementSpread {
		t.Fatalf("strategy = %q, want the default", stored.PlacementStrategy)
	}
	got, err := st.DeploymentRequirements(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := nodelabel.Requirements{
		nodelabel.KeyNode:   {"n-1", "n-2"},
		nodelabel.KeyVendor: {"nvidia"},
		"pool":              {"inference"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}

	binpack, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "b", PlacementStrategy: PlacementBinpack}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := st.GetDeployment(binpack.ID); stored.PlacementStrategy != PlacementBinpack {
		t.Fatalf("strategy = %q, want the stated one", stored.PlacementStrategy)
	}
	if got, err := st.DeploymentRequirements(binpack.ID); err != nil || got != nil {
		t.Fatalf("requirements of a deployment stating none = %v, %v", got, err)
	}
}

// Invalid requirements are refused before anything is written: there is no
// deployment left behind without the requirements it was created with.
func TestCreateDeploymentWithInvalidRequirementsWritesNothing(t *testing.T) {
	st := newTestStore(t)
	_, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "a"}, nodelabel.Requirements{
		nodelabel.Prefix + "gpu_model": {"A100"},
	})
	if err == nil || !strings.Contains(err.Error(), "not a system label") {
		t.Fatalf("err = %v", err)
	}
	deps, err := st.ListDeployments()
	if err != nil || len(deps) != 0 {
		t.Fatalf("deployments = %v, %v; want none", deps, err)
	}
}

// The table exists to answer "who names this node or network" by query, and
// deleting a deployment removes its rows with it.
func TestDeploymentsRequiringAndDelete(t *testing.T) {
	st := newTestStore(t)
	create := func(name string, reqs nodelabel.Requirements) uint {
		t.Helper()
		dep, err := st.CreateDeploymentWithRequirements(&Deployment{Name: name}, reqs)
		if err != nil {
			t.Fatal(err)
		}
		return dep.ID
	}
	a := create("a", nodelabel.Requirements{nodelabel.KeyComputeNetwork: {"7"}, nodelabel.KeyNode: {"n-1"}})
	b := create("b", nodelabel.Requirements{nodelabel.KeyComputeNetwork: {"7", "8"}})
	create("c", nodelabel.Requirements{nodelabel.KeyServingNetwork: {"7"}})

	ids, err := st.DeploymentsRequiring(nodelabel.KeyComputeNetwork, "7")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []uint{a, b}) {
		t.Fatalf("deployments naming compute network 7 = %v, want [%d %d]", ids, a, b)
	}
	all, err := st.DeploymentRequirementsFor([]uint{a, b, 999})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || !reflect.DeepEqual(all[b][nodelabel.KeyComputeNetwork], []string{"7", "8"}) {
		t.Fatalf("batch = %v", all)
	}

	if err := st.DeleteDeployment(a); err != nil {
		t.Fatal(err)
	}
	if ids, _ := st.DeploymentsRequiring(nodelabel.KeyNode, "n-1"); len(ids) != 0 {
		t.Fatalf("deleted deployment still names n-1: %v", ids)
	}
	var left int64
	if err := st.db.Model(&DeploymentNodeRequirement{}).Where("deployment_id = ?", a).Count(&left).Error; err != nil || left != 0 {
		t.Fatalf("rows left = %d, %v", left, err)
	}
}

// Replacing requirements swaps the whole set and the strategy in one write;
// requirements that cannot be stored, or a deployment that is gone, change
// nothing.
func TestSetDeploymentRequirements(t *testing.T) {
	st := newTestStore(t)
	dep, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "a"}, nodelabel.Requirements{
		nodelabel.KeyVendor: {"nvidia"}, nodelabel.KeyNode: {"n-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "b"}, nodelabel.Requirements{"pool": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentRequirements(dep.ID, nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, "pool": {"b", "a", "b"}}, PlacementBinpack); err != nil {
		t.Fatal(err)
	}
	got, _ := st.DeploymentRequirements(dep.ID)
	if want := (nodelabel.Requirements{nodelabel.KeyVendor: {"nvidia"}, "pool": {"a", "b"}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	if row, _ := st.GetDeployment(dep.ID); row.PlacementStrategy != PlacementBinpack {
		t.Fatalf("strategy = %q", row.PlacementStrategy)
	}
	if got, _ := st.DeploymentRequirements(other.ID); !reflect.DeepEqual(got, nodelabel.Requirements{"pool": {"x"}}) {
		t.Fatalf("another deployment's requirements changed: %v", got)
	}

	if err := st.SetDeploymentRequirements(dep.ID, nodelabel.Requirements{"gpuworks.ai/gpu_model": {"H100"}}, PlacementSpread); err == nil {
		t.Fatal("a misspelt system key was stored")
	}
	if err := st.SetDeploymentRequirements(9999, nodelabel.Requirements{"pool": {"a"}}, PlacementSpread); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment = %v, want ErrNotFound", err)
	}
	if ids, _ := st.DeploymentsRequiring("pool", "a"); !reflect.DeepEqual(ids, []uint{dep.ID}) {
		t.Fatalf("a refused write left rows behind: %v", ids)
	}
	// An empty set is a deployment that may run anywhere; the default
	// strategy fills an unstated one.
	if err := st.SetDeploymentRequirements(dep.ID, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.DeploymentRequirements(dep.ID); len(got) != 0 {
		t.Fatalf("requirements = %v, want none", got)
	}
	if row, _ := st.GetDeployment(dep.ID); row.PlacementStrategy != PlacementSpread {
		t.Fatalf("strategy = %q, want the default", row.PlacementStrategy)
	}
}

// A served name is held by every deployment not being deleted. A create
// asking for a held one is refused before anything is written, its
// requirements included; rows that already share a name stay as they are.
func TestCreateDeploymentRefusesAServedNameInUse(t *testing.T) {
	st := newTestStore(t)
	active, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "active", ServedName: "qwen3-0.6b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "stopped", ServedName: "stopped-name", State: DeployStopped}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "deleting", ServedName: "deleting-name", State: DeployDeleting}, nil); err != nil {
		t.Fatal(err)
	}
	before, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ served, holder string }{{"qwen3-0.6b", "active"}, {"stopped-name", "stopped"}} {
		_, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "again", ServedName: tc.served}, nodelabel.Requirements{"pool": {"inference"}})
		var inUse *ServedNameInUseError
		if !errors.As(err, &inUse) || inUse.ServedName != tc.served || inUse.HolderName != tc.holder || inUse.HolderID == 0 {
			t.Fatalf("create under %q: err = %v, want held by %s", tc.served, err, tc.holder)
		}
	}
	after, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused creates wrote %d deployments", len(after)-len(before))
	}
	var requirementRows int64
	if err := st.DB().Model(&DeploymentNodeRequirement{}).Count(&requirementRows).Error; err != nil || requirementRows != 0 {
		t.Fatalf("refused creates left %d requirement rows (err %v)", requirementRows, err)
	}

	// A deployment being deleted gives its name up, and a name nobody holds
	// is free; an empty served name is not a name to hold.
	for _, served := range []string{"deleting-name", "fresh-name", "", ""} {
		if _, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "new", ServedName: served}, nil); err != nil {
			t.Fatalf("create under %q: %v", served, err)
		}
	}

	// Rows written before the rule may share a name; they are left alone and
	// still hold it.
	if err := st.DB().Create(&Deployment{Name: "legacy", ServedName: active.ServedName, State: DeployActive}).Error; err != nil {
		t.Fatal(err)
	}
	if shared, err := st.DeploymentsByServedName(active.ServedName); err != nil || len(shared) != 2 {
		t.Fatalf("legacy shared rows = %d, %v", len(shared), err)
	}
	var inUse *ServedNameInUseError
	if _, err := st.CreateDeploymentWithRequirements(&Deployment{Name: "third", ServedName: active.ServedName}, nil); !errors.As(err, &inUse) || inUse.HolderID != active.ID {
		t.Fatalf("create beside legacy sharers: err = %v, want held by the oldest (%d)", err, active.ID)
	}
}
