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
	r.Wait(2, Cards{NodeID: 7, GPUIndexes: []int{0}}, []string{"b"}, t0.Add(time.Second))
	r.Wait(1, Cards{NodeID: 7, GPUIndexes: []int{1}}, []string{"a"}, t0.Add(2*time.Second))
	// Noted again: it keeps its start and takes the new cards and holders.
	holders, indexes := []string{"b", "c"}, []int{0, 2}
	r.Wait(2, Cards{NodeID: 7, GPUIndexes: indexes}, holders, t0.Add(5*time.Second))
	holders[0], indexes[0] = "changed", 9
	got := r.Waiting(t0.Add(6 * time.Second))
	if len(got) != 2 || got[0].DeploymentID != 2 || got[1].DeploymentID != 1 ||
		!got[0].Since.Equal(t0.Add(time.Second)) || !slices.Equal(got[0].Holders, []string{"b", "c"}) ||
		!slices.Equal(got[0].Cards.GPUIndexes, []int{0, 2}) {
		t.Fatalf("waiting = %+v", got)
	}
	if !slices.Equal(told, []uint{2, 1}) {
		t.Fatalf("told %v, want each deployment once, as it starts waiting", told)
	}
	got[0].Holders[0], got[0].Cards.GPUIndexes[0] = "edited", 9
	if w, ok := r.Get(2, t0.Add(6*time.Second)); !ok || w.Holders[0] != "b" || w.Cards.GPUIndexes[0] != 0 {
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

// Once the holders' sleeps are ordered the controller notes the wait with no
// holder left to put to sleep; the cards are still the waiter's.
func TestAWaitForOrderedSleepsKeepsItsCards(t *testing.T) {
	r := New()
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	r.Wait(3, Cards{NodeID: 4, GPUIndexes: []int{1}}, []string{"h-1"}, t0)
	r.Wait(3, Cards{NodeID: 4, GPUIndexes: []int{1}}, nil, t0.Add(Recheck))
	w, ok := r.Get(3, t0.Add(Recheck))
	if !ok || len(w.Holders) != 0 || !w.Cards.Takes(4, 1) || w.Cards.Takes(4, 0) || w.Cards.Takes(5, 1) {
		t.Fatalf("wait = %+v %v, want card 1 of node 4 still its", w, ok)
	}
}

// A replica the controller plans every Recheck is noted again long before
// its note goes stale, even across a slow pass or two.
func TestANoteOutlivesSeveralRechecks(t *testing.T) {
	if Fresh < 3*Recheck {
		t.Fatalf("Fresh %v is within three rechecks (%v): a slow pass drops a wait", Fresh, Recheck)
	}
}
