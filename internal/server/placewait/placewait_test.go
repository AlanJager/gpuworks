package placewait

import (
	"slices"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	r := New()
	var told []uint
	r.OnWait(func(id uint) { told = append(told, id) })
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	r.Wait(2, []string{"b"}, t0.Add(time.Second))
	r.Wait(1, []string{"a"}, t0.Add(2*time.Second))
	// Noted again: it keeps its start and takes the new instances.
	instances := []string{"b", "c"}
	r.Wait(2, instances, t0.Add(5*time.Second))
	instances[0] = "changed"
	got := r.Waiting(t0.Add(6 * time.Second))
	if len(got) != 2 || got[0].DeploymentID != 2 || got[1].DeploymentID != 1 ||
		!got[0].Since.Equal(t0.Add(time.Second)) || !slices.Equal(got[0].Instances, []string{"b", "c"}) {
		t.Fatalf("waiting = %+v", got)
	}
	if !slices.Equal(told, []uint{2, 1}) {
		t.Fatalf("told %v, want each deployment once, as it starts waiting", told)
	}
	got[0].Instances[0] = "edited"
	if w, ok := r.Get(2, t0.Add(6*time.Second)); !ok || w.Instances[0] != "b" {
		t.Fatalf("get = %+v %v: a caller's copy reached the registry", w, ok)
	}
	// A note the controller stops making goes stale.
	if _, ok := r.Get(1, t0.Add(2*time.Second+Fresh)); ok {
		t.Fatal("a stale note still reads as waiting")
	}
	if got := r.Waiting(t0.Add(2*time.Second + Fresh)); len(got) != 1 || got[0].DeploymentID != 2 {
		t.Fatalf("after 1 went stale = %+v", got)
	}
	r.Done(2)
	if got := r.Waiting(t0.Add(10 * time.Second)); len(got) != 0 {
		t.Fatalf("after done = %+v", got)
	}
}
