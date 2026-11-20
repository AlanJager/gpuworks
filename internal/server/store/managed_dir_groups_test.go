package store

import (
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The replay names exactly the groups whose deployment is gone: deleted, never
// existing in this database, or an id now held by a deployment created at
// another second. A live deployment's group - including one being deleted -
// is kept, and what is not a group key is never named.
func TestReleasedManagedDirGroupsNamesOnlyGroupsOfGoneDeployments(t *testing.T) {
	st := newTestStore(t)
	live, err := st.CreateDeployment(&Deployment{Name: "live"})
	if err != nil {
		t.Fatal(err)
	}
	deleting, err := st.CreateDeployment(&Deployment{Name: "deleting", State: DeployDeleting})
	if err != nil {
		t.Fatal(err)
	}
	gone, err := st.CreateDeployment(&Deployment{Name: "gone"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteDeployment(gone.ID); err != nil {
		t.Fatal(err)
	}
	group := func(dep *Deployment) string { return proto.ManagedDirGroupKey(dep.ID, dep.CreatedAt.Unix()) }
	reused := proto.ManagedDirGroupKey(live.ID, live.CreatedAt.Unix()-3600)
	never := proto.ManagedDirGroupKey(live.ID+1000, live.CreatedAt.Unix())

	released, err := st.ReleasedManagedDirGroups([]string{group(live), group(deleting), group(gone), reused, never, "not-a-group", "../1-2"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{group(gone), reused, never}; !reflect.DeepEqual(released, want) {
		t.Fatalf("released = %v, want %v", released, want)
	}
	if released, err := st.ReleasedManagedDirGroups(nil); err != nil || released != nil {
		t.Fatalf("nothing held = %v %v", released, err)
	}
}
