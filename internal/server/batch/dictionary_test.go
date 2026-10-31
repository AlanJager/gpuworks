package batch

import (
	"errors"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// The dictionary states what the create checks: the default window is the one
// a batch gets unasked, every window it offers parses and none outside does,
// the statuses are every status once, and the models are the lookup's list.
func TestDictionaryIsWhatTheCreateChecks(t *testing.T) {
	r := newRig(t, Config{Models: platformModels, OutputRetention: 7 * 24 * time.Hour}, nil)
	d, err := r.svc.Dictionary()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ParseWindow(CompletionWindow); !ok || got != time.Duration(d.DefaultWindowHours)*time.Hour {
		t.Fatalf("default window %dh, but a batch asking for none gets %s", d.DefaultWindowHours, CompletionWindow)
	}
	for _, hours := range []int{d.MinWindowHours, d.MaxWindowHours} {
		if _, ok := ParseWindow(itoaHours(hours)); !ok {
			t.Errorf("the offered bound %dh is refused", hours)
		}
	}
	for _, hours := range []int{d.MinWindowHours - 1, d.MaxWindowHours + 1} {
		if _, ok := ParseWindow(itoaHours(hours)); ok {
			t.Errorf("%dh, outside the offered bounds, is accepted", hours)
		}
	}
	if d.MaxFileBytes != MaxFileBytes || d.MaxFileLines != MaxFileLines || !reflect.DeepEqual(d.Endpoints, Endpoints) || d.OutputRetention != 7*24*time.Hour {
		t.Fatalf("dictionary = %+v", d)
	}
	all := []string{StatusValidating, StatusInProgress, StatusFinalizing, StatusCompleted, StatusFailed, StatusExpired, StatusCancelling, StatusCancelled}
	got := append([]string(nil), d.Statuses...)
	sort.Strings(got)
	sort.Strings(all)
	if !reflect.DeepEqual(got, all) {
		t.Fatalf("statuses = %v", d.Statuses)
	}
	for i, status := range d.Statuses {
		if Terminal(status) && i < len(d.Statuses)-1 && !Terminal(d.Statuses[i+1]) {
			t.Fatalf("a moving status follows a terminal one: %v", d.Statuses)
		}
	}
	want, _ := platformModels.List()
	if !reflect.DeepEqual(d.Models, want) {
		t.Fatalf("models = %+v, want the lookup's %+v", d.Models, want)
	}
}

func itoaHours(n int) string { return strconv.Itoa(n) + "h" }

// Without a lookup no model is listed (and none is checked); a failing lookup
// fails the dictionary rather than listing nothing.
func TestDictionaryModelsWithoutOrFailingALookup(t *testing.T) {
	r := newRig(t, Config{}, nil)
	d, err := r.svc.Dictionary()
	if err != nil || d.Models == nil || len(d.Models) != 0 {
		t.Fatalf("without a lookup = %+v err=%v", d.Models, err)
	}
	r = newRig(t, Config{Models: fakeModels{listErr: errors.New("store is down")}}, nil)
	if _, err := r.svc.Dictionary(); err == nil {
		t.Fatal("a failing list gave a dictionary")
	}
}

// A model asleep in a way a request wakes is accepted like an awake one; a
// held one only with a warning; ranking puts awake first.
func TestModelStatesAcceptAndRank(t *testing.T) {
	if !ModelAvailable.Accepts() || !ModelAsleep.Accepts() || ModelSleepHeld.Accepts() || ModelUnknown.Accepts() {
		t.Fatal("accepts")
	}
	order := []ModelState{ModelAvailable, ModelAsleep, ModelSleepHeld, ModelUnknown}
	for i := 0; i+1 < len(order); i++ {
		if !order[i].Better(order[i+1]) || order[i+1].Better(order[i]) {
			t.Fatalf("%v should rank above %v", order[i], order[i+1])
		}
	}
	r := newRig(t, Config{Models: fakeModels{states: map[string]ModelState{"napping": ModelAsleep}}}, nil)
	if warnings, err := r.svc.checkModel("napping"); err != nil || len(warnings) != 0 {
		t.Fatalf("an asleep model a request wakes = %v %v", warnings, err)
	}
}
