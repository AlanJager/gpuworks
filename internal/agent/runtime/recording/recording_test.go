package recording

import (
	"context"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// The orphan sweeper compares ListContainers against the desired set and then
// calls Stop/Remove with the reported names, so a listing that disagrees with
// Status would either leak containers or try to delete workloads that do not
// exist. Keep the two views of the same state in step.
func TestListContainersMatchesStatusAndDropsRemovedContainers(t *testing.T) {
	d := New(t.TempDir() + "/spec.json")
	ctx := context.Background()
	if names, err := d.ListContainers(ctx); err != nil || len(names) != 0 {
		t.Fatalf("fresh listing = %v, err=%v", names, err)
	}
	first, err := d.Run(ctx, runtime.WorkloadSpec{Name: "gpuworks-bbb", Image: "image:1"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Run(ctx, runtime.WorkloadSpec{Name: "gpuworks-aaa", Image: "image:2"})
	if err != nil {
		t.Fatal(err)
	}
	names, err := d.ListContainers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{first, second}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("listing = %v, want %v", names, want)
	}
	for _, name := range names {
		st, err := d.Status(ctx, name)
		if err != nil || !st.Exists {
			t.Fatalf("listed container %s has status %+v, err=%v", name, st, err)
		}
	}
	if err := d.Remove(ctx, first); err != nil {
		t.Fatal(err)
	}
	if names, err = d.ListContainers(ctx); err != nil || !slices.Equal(names, []string{second}) {
		t.Fatalf("listing after Remove = %v, err=%v", names, err)
	}
}

// The liveness monitor skips the inspect of every container the running
// listing names, so the listing must name exactly the containers Status
// reports running.
func TestRunningContainersNamesExactlyWhatStatusReportsRunning(t *testing.T) {
	d := New(t.TempDir() + "/spec.json")
	ctx := context.Background()
	if names, err := d.RunningContainers(ctx); err != nil || len(names) != 0 {
		t.Fatalf("fresh listing = %v, err=%v", names, err)
	}
	up, err := d.Run(ctx, runtime.WorkloadSpec{Name: "gpuworks-up", Image: "image:1"})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := d.Run(ctx, runtime.WorkloadSpec{Name: "gpuworks-stopped", Image: "image:1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Stop(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	names, err := d.RunningContainers(ctx)
	if err != nil || !slices.Equal(names, []string{up}) {
		t.Fatalf("running listing = %v, err=%v, want only %s", names, err, up)
	}
	for _, id := range []string{up, stopped} {
		st, err := d.Status(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if listed := slices.Contains(names, id); listed != (st.Exists && st.Running) {
			t.Fatalf("%s: listed=%v but Status=%+v", id, listed, st)
		}
	}
}
